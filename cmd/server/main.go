// fobe-server: the panel server + scheduler + admin CLI (design.md §4.1, §17).
//
// Usage:
//
//	fobe-server                     # run the panel (env: FOBE_MASTER_KEY, FOBE_DB, …)
//	fobe-server admin unblock <ip|all>
//	fobe-server admin reset-password
//	fobe-server admin list-sessions [--revoke]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fonlan/fobe/internal/server/agentupdate"
	"github.com/fonlan/fobe/internal/server/feishureg"
	"github.com/fonlan/fobe/internal/server/geoip"
	"github.com/fonlan/fobe/internal/server/geoipupdate"
	"github.com/fonlan/fobe/internal/server/httpapi"
	"github.com/fonlan/fobe/internal/server/hub"
	"github.com/fonlan/fobe/internal/server/modelsdev"
	"github.com/fonlan/fobe/internal/server/notify"
	"github.com/fonlan/fobe/internal/server/scheduler"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singboxcache"
	"github.com/fonlan/fobe/internal/server/singboxdl"
	"github.com/fonlan/fobe/internal/server/store"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		runAdmin(os.Args[2:])
		return
	}
	runServer()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dbPath() string { return env("FOBE_DB", "/data/fobe.db") }

func mustStore(path string) *store.Store {
	st, err := store.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fobe-server: open db: %v\n", err)
		os.Exit(1)
	}
	return st
}

// requireMasterKey enforces design §4.4: no master key → refuse to start
// (never silently degrade to plaintext).
func mustMasterKey() []byte {
	v := os.Getenv("FOBE_MASTER_KEY")
	if v == "" {
		fmt.Fprintln(os.Stderr, "fobe-server: FOBE_MASTER_KEY is required (AI keys, bot tokens and other sensitive settings are encrypted with it).")
		fmt.Fprintln(os.Stderr, "  generate one with:  openssl rand -base64 32")
		os.Exit(1)
	}
	key, err := security.ParseMasterKey(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fobe-server: FOBE_MASTER_KEY: %v (generate with: openssl rand -base64 32)\n", err)
		os.Exit(1)
	}
	return key
}

func runServer() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	key := mustMasterKey()
	st := mustStore(dbPath())
	defer st.Close()

	// Startup work that must not block serving (the sing-box artifact
	// download) hangs off this context and stops with the server.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	crypt, err := security.NewCryptor(key)
	if err != nil {
		log.Error("master key", "err", err)
		os.Exit(1)
	}
	trust, err := security.NewTrustChain(env("FOBE_TRUSTED_PROXIES", ""))
	if err != nil {
		log.Error("FOBE_TRUSTED_PROXIES", "err", err)
		os.Exit(1)
	}

	ensureAdminUser(st, log)

	// GeoIP country resolution (design §14): local GeoLite2-Country MMDB at
	// FOBE_GEOIP_MMDB, online fallback opt-in via FOBE_GEOIP_ONLINE=1 (it
	// sends node IPs to a third-party API, so it stays off by default).
	geoPath := env("FOBE_GEOIP_MMDB", geoip.DefaultMMDBPath)
	geoOnline, _ := strconv.ParseBool(env("FOBE_GEOIP_ONLINE", "0"))
	// One MMDB handle for the whole process: the §14.1 updater reloads it after
	// a download, and a second handle would keep answering from the old file
	// until its own stat check happened to fire.
	mmdb := geoip.NewMMDB(geoPath)
	h := hub.New(st, trust, log, geoip.NewWith(mmdb, geoOnline))
	// §9.3 实现修订 2026-09-17: the local-discovery snapshot is an operator's own
	// config.json and may carry credentials, so the hub stores it encrypted.
	h.SetCryptor(crypt)
	log.Info("geoip resolver", "mmdb", geoPath, "online_fallback", geoOnline)
	api := httpapi.NewServer(st, h, trust, crypt, log)
	api.Version = version
	api.WebDir = env("FOBE_WEB_DIR", "")
	dlDir := env("FOBE_DL_DIR", "")
	api.DLDir = dlDir
	// Release source overrides (§9.2): mirrors and air-gapped installs point
	// these at a GitHub-compatible API / download root.
	api.SingboxAPIBase = env("FOBE_SINGBOX_API_BASE", "")
	api.SingboxDownloadBase = env("FOBE_SINGBOX_DOWNLOAD_BASE", "")
	api.InstallTmplPath = os.Getenv("FOBE_INSTALL_TMPL")
	// The settings page shows this switch next to the artifact cache (§9.2).
	api.SingboxAutoDownload = singboxAutoDownloadEnabled()
	// Long-lived context for work a handler starts but does not wait for.
	api.Background = bgCtx
	// The MMDB upload endpoint (§14) writes geoPath and Reload()s this handle
	// right away; the hub's chain resolver shares the very same handle, so a
	// new database serves lookups at once instead of at the next stat check.
	api.GeoIPMMDBPath = geoPath
	api.GeoIPResolver = mmdb
	// Automatic database refresh (§14.1): keyless mirrors, a daily freshness
	// check and the panel's manual update button all drive this one manager.
	// FOBE_GEOIP_URL pins a private mirror, FOBE_GEOIP_AUTO_UPDATE=0 turns every
	// automatic download off (the manual button keeps working).
	api.GeoIPUpdater = geoipupdate.New(geoipupdate.Config{
		Path:       geoPath,
		Settings:   st,
		Log:        log,
		SourceURL:  env("FOBE_GEOIP_URL", ""),
		AutoUpdate: geoipAutoUpdateEnabled(),
		Reload:     mmdb.Reload,
		Progress:   api.OnGeoIPProgress,
	})
	api.GeoIPUpdater.Start(bgCtx)

	// models.dev metadata (§12.5): a typed api.json index cached under the data
	// volume and refreshed daily. FOBE_MODELS_URL points at a mirror,
	// FOBE_MODELS_AUTO_UPDATE=0 disables the automatic refresh (the panel's
	// manual button keeps working). Start reads the local cache synchronously
	// and only touches the network in the background, so a panel with no egress
	// still comes up — with empty metadata and hand-entered models.
	api.ModelsDev = modelsdev.New(modelsdev.Config{
		Dir: filepath.Dir(dbPath()),
		Log: log,
	})
	api.ModelsDev.Start(bgCtx)

	stop := make(chan struct{})
	notifyClient := &http.Client{Timeout: notify.DeliveryTimeout}
	decryptSetting := func(key string) (string, bool) {
		val, encrypted, err := st.GetSettingValue(key)
		if err != nil || val == "" {
			return "", false
		}
		if encrypted {
			plain, err := crypt.Decrypt(val)
			if err != nil {
				log.Warn("decrypt setting", "key", key, "err", err)
				return "", false
			}
			val = plain
		}
		return val, true
	}
	feishu := notify.NewFeishu(decryptSetting, notifyClient)
	channels := []notify.Notifier{
		notify.NewTelegram(decryptSetting, notifyClient),
		notify.NewWebhook(decryptSetting, notifyClient),
		feishu,
	}
	// One notifier set, two consumers: the scheduler delivers with it and the
	// notifications page's test button sends through the same instances.
	api.Channels = channels
	// §15 飞书扫码接入 (2026-09-16 修订): one shared Feishu channel + its
	// registration manager. The Save closure persists the fresh app's
	// credentials (secret encrypted at rest), pins the scanning user as the
	// receive target, audits the binding and says hello in Feishu — the
	// channel being alive is the user's confirmation that the scan worked.
	api.Feishu = feishu
	api.FeishuReg = feishureg.New(feishureg.Config{
		Log:     log,
		AppName: "fobe 探针告警",
		AppDesc: "接收 fobe 探针面板的告警与恢复通知",
		Save: func(appID, appSecret, openID, botName, domain string) error {
			enc, err := crypt.Encrypt(appSecret)
			if err != nil {
				return err
			}
			for _, kv := range []struct {
				key string
				val string
				enc bool
			}{
				{"notify.feishu_app_id", appID, false},
				{"notify.feishu_app_secret", enc, true},
				{"notify.feishu_receive_id", openID, false},
				{"notify.feishu_bot_name", botName, false},
				{"notify.feishu_domain", domain, false},
			} {
				if err := st.SetSetting(kv.key, kv.val, kv.enc); err != nil {
					return err
				}
			}
			_ = st.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "feishu_qr_saved", Command: botName})
			// Rendered by the same function the real alerts use, so the
			// welcome is also a sample of what the user signed up for.
			welcome := notify.MessageText(
				notify.Event{Event: notify.EventTest, CreatedAt: time.Now().Unix()},
				notify.TextOptionsFor(decryptSetting),
			)
			if err := feishu.SendText(welcome + "\nFeishu notification channel connected."); err != nil {
				log.Warn("feishu welcome send", "err", err)
			}
			return nil
		},
	})
	// Backups are on by default (snapshots land next to the database, §17) —
	// the 2026-09-16 corruption incident cost the settings table because the
	// only copy of the data was the data itself. FOBE_BACKUP_DIR relocates
	// them; FOBE_BACKUP_DIR=off is the explicit opt-out.
	backupDir := env("FOBE_BACKUP_DIR", filepath.Join(filepath.Dir(dbPath()), "backup"))
	if backupDir == "off" || backupDir == "none" {
		backupDir = ""
	}
	sched := scheduler.New(st, log, backupDir, 7, channels...)
	sched.Start(stop)
	go h.PumpCommands(2 * time.Second)

	// Server-side sing-box artifact cache (§9.2): download the current stable
	// release when the cache is empty, record the verdict in settings for the
	// panel, and warn when the artifact directory would not survive a container
	// upgrade. It never blocks startup.
	api.SingboxCache = startSingboxCache(bgCtx, st, log, api.SingboxDL())

	// §5.5 agent self-update. Two startup steps, in this order:
	//   1. seed the artifact volume with the agent builds that shipped inside
	//      the image — the compose bind mount hides the image's own /srv/dl, so
	//      without this copy the panel has nothing to hand out and /install.sh
	//      404s on a fresh deployment;
	//   2. hand the manager to the hub (target + stagger in hello_ack) and to
	//      the API (status, operator retry, reinstall command).
	seedDir := env("FOBE_AGENT_SEED_DIR", agentupdate.DefaultSeedDir)
	if dlDir != "" {
		if err := agentupdate.Seed(seedDir, dlDir, version, log); err != nil {
			log.Warn("agent artifact seed failed", "seed_dir", seedDir, "dl_dir", dlDir, "err", err)
		}
	}
	// §5.5 stagger: the five-minute deterministic spread is what keeps a
	// post-restart stampede off the artifact volume — but a dev/test box with
	// one or two probes has no stampede to spread, and waiting out the window
	// makes verification look like "nothing happened". FOBE_AGENT_UPDATE_STAGGER
	// (1s = immediate: the offset is hash%span) is the escape hatch; unset keeps
	// the default. Values <= 0 are refused because agentupdate.New reads them as
	// "unset" and would silently put the default back.
	stagger := agentupdate.DefaultStagger
	if v := strings.TrimSpace(os.Getenv("FOBE_AGENT_UPDATE_STAGGER")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			stagger = d
		} else {
			log.Warn("FOBE_AGENT_UPDATE_STAGGER ignored",
				"value", v, "using", agentupdate.DefaultStagger.String())
		}
	}
	agentUpd := agentupdate.New(agentupdate.Config{
		Store:         st,
		Log:           log,
		ServerVersion: version,
		DLDir:         dlDir,
		Stagger:       stagger,
		Enabled:       func() bool { return agentupdate.AutoUpdateEnabled(st) },
	})
	h.SetAgentUpdater(agentUpd)
	api.AgentUpdate = agentUpd
	agentUpd.Start(bgCtx)

	// §9.2 one-click batch update: build the manager now and re-arm the
	// convergence check of a job that was still pending when the process last
	// stopped (the deadline is persisted with the job).
	api.SingboxUpdater().Start(bgCtx)

	// §9.1: a node's config is generated once and then cached in settings, so a
	// change to the *generator* would otherwise never reach existing nodes —
	// they would keep pushing the stale bytes and the agent (which only
	// compares hashes) would have no reason to rewrite the file. Rebuild what
	// no longer matches the current template, before anyone can be served.
	api.SyncSingboxConfigs()

	// §10 实现修订 2026-09-16b: the {{rules}} placeholder and the settings that
	// filled it are gone — rules live in the template now. Templates written
	// before that still carry the token, so inline the old snippets once here:
	// a subscription that rendered fine yesterday must not start serving a
	// literal "{{rules}}" today. Idempotent, and it drops the dead settings.
	if n := api.MigrateLegacyRules(); n > 0 {
		log.Info("inlined legacy {{rules}} snippets into templates", "templates", n)
	}

	// §10.2: relay entries are derived from the probes' nftables forwards, so
	// the rows an upgrade inherits must be reconciled before the first fetch —
	// a topology that already existed would otherwise only appear the next time
	// a probe re-reports its rules. Idempotent; tombstones are never revived.
	if n := api.ReconcileSubscriptionEntries(); n > 0 {
		log.Info("auto-enrolled relay subscription entries", "count", n)
	}

	// §17: optional read-only profiling endpoint (off unless FOBE_PPROF is a
	// loopback address). It also feeds scripts/pgo.sh.
	startPprof(log)

	addr := env("FOBE_LISTEN", "0.0.0.0:8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("fobe-server listening", "addr", addr, "version", version,
			"web_dir", api.WebDir, "dl_dir", api.DLDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down")
	bgCancel() // abort an in-flight sing-box download
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// startSingboxCache wires the server-side sing-box artifact cache (design
// §9.2, §17).
//
// When <FOBE_DL_DIR>/singbox holds no valid version and auto-download is on,
// the current stable release is fetched in the background: a slow or
// unreachable release host must never delay the panel, and a warm cache never
// touches the network at all. The outcome lands in settings
// (singbox.cache_status) so the panel can show the state, the cached version
// and the failure reason, and offer a manual retry; a container whose
// FOBE_DL_DIR is not on a mount of its own is flagged in
// singbox.dl_mount_ok (upgrading such a container loses downloaded versions).
//
// dl is the process-wide artifact client (httpapi.Server.SingboxDL): the
// startup download must share it with the batch updater, because singboxdl
// serializes Install per client — two clients would download the same tarball
// twice (design §9.5.3).
func startSingboxCache(ctx context.Context, st *store.Store, log *slog.Logger, dl *singboxdl.Client) *singboxcache.Manager {
	mgr := singboxcache.New(singboxcache.Config{
		DL:           dl,
		Settings:     st,
		Log:          log,
		AutoDownload: singboxAutoDownloadEnabled(),
	})
	mgr.Start(ctx)
	return mgr
}

// settingBool reads a boolean setting the way the panel writes it ("1"/"0",
// "true"/"false"; absent = false).
func settingBool(st *store.Store, key string) bool {
	v, err := st.GetSetting(key)
	if err != nil {
		return false
	}
	v = strings.TrimSpace(v)
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return v == "1"
}

// singboxAutoDownloadEnabled reports whether the startup download runs.
// FOBE_SINGBOX_AUTO_DOWNLOAD=0 (or false/off/no) turns it off; anything else,
// including unset, keeps the default on (design §9.2).
func singboxAutoDownloadEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FOBE_SINGBOX_AUTO_DOWNLOAD"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// geoipAutoUpdateEnabled reports whether automatic GeoIP database refreshes may
// run (design §14.1). FOBE_GEOIP_AUTO_UPDATE=0 (or false/off/no) turns them
// off, including the startup download of a missing database; the panel's
// "update now" button and an upload still work. Anything else, including
// unset, keeps the default on — the panel's own switch is the softer control.
func geoipAutoUpdateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FOBE_GEOIP_AUTO_UPDATE"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// ensureAdminUser makes FOBE_ADMIN_PASSWORD the source of truth for the
// admin password: on first boot it seeds the user (design §19.11); on every
// later boot it syncs the password to the env value, revoking stale sessions
// when it actually changed. Unset env leaves the existing password alone —
// recovery stays possible via `fobe-server admin reset-password`.
func ensureAdminUser(st *store.Store, log *slog.Logger) {
	envPassword := os.Getenv("FOBE_ADMIN_PASSWORD")

	user, err := st.GetUser()
	switch {
	case err == nil:
		// user exists: sync the password when the env value differs
		if envPassword == "" {
			return
		}
		if security.VerifyPassword(envPassword, user.PasswordHash) {
			if user.MustChange {
				// seeded from this same env earlier and never changed by hand
				_ = st.UpdatePassword(user.ID, user.PasswordHash, false)
			}
			return
		}
		hash, err := security.HashPassword(envPassword)
		if err != nil {
			log.Error("hash password", "err", err)
			os.Exit(1)
		}
		if err := st.UpdatePassword(user.ID, hash, false); err != nil {
			log.Error("update password", "err", err)
			os.Exit(1)
		}
		if n, err := st.RevokeAllSessions(); err != nil {
			log.Warn("revoke sessions", "err", err)
		} else if n > 0 {
			log.Info("revoked sessions after password rotation", "count", n)
		}
		log.Info("admin password synced from FOBE_ADMIN_PASSWORD")

	case errors.Is(err, store.ErrNotFound):
		password := envPassword
		mustChange := false
		if password == "" {
			generated, err := security.RandomToken(12)
			if err != nil {
				log.Error("generate password", "err", err)
				os.Exit(1)
			}
			password = generated
			mustChange = true
			log.Warn("INITIAL PASSWORD (one time, change it after login): " + password)
		} else {
			log.Info("admin user created from FOBE_ADMIN_PASSWORD")
		}
		hash, err := security.HashPassword(password)
		if err != nil {
			log.Error("hash password", "err", err)
			os.Exit(1)
		}
		if _, err := st.CreateUser(hash, mustChange); err != nil {
			log.Error("create user", "err", err)
			os.Exit(1)
		}

	default:
		log.Error("read user", "err", err)
		os.Exit(1)
	}
}

// --- admin CLI (design §4.1: the escape hatch that never needs the web) ---

func runAdmin(args []string) {
	if len(args) == 0 {
		adminUsage()
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	switch args[0] {
	case "unblock":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: fobe-server admin unblock <ip|all>")
			os.Exit(2)
		}
		st := mustStore(dbPath())
		defer st.Close()
		if err := st.UnblockIP(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "unblock: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("unblocked %s\n", args[1])

	case "reset-password":
		st := mustStore(dbPath())
		defer st.Close()
		pw, err := security.RandomToken(12)
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate: %v\n", err)
			os.Exit(1)
		}
		hash, err := security.HashPassword(pw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hash: %v\n", err)
			os.Exit(1)
		}
		user, err := st.GetUser()
		if errors.Is(err, store.ErrNotFound) {
			if _, err := st.CreateUser(hash, true); err != nil {
				fmt.Fprintf(os.Stderr, "create user: %v\n", err)
				os.Exit(1)
			}
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "read user: %v\n", err)
			os.Exit(1)
		} else if err := st.UpdatePassword(user.ID, hash, true); err != nil {
			fmt.Fprintf(os.Stderr, "update: %v\n", err)
			os.Exit(1)
		}
		if _, err := st.RevokeAllSessions(); err != nil {
			log.Warn("revoke sessions", "err", err)
		}
		fmt.Println("NEW PASSWORD (one time, change it after login):")
		fmt.Println(pw)

	case "list-sessions":
		fs := flag.NewFlagSet("list-sessions", flag.ContinueOnError)
		revoke := fs.Bool("revoke", false, "revoke all sessions")
		_ = fs.Parse(args[1:])
		st := mustStore(dbPath())
		defer st.Close()
		if *revoke {
			n, err := st.RevokeAllSessions()
			if err != nil {
				fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("revoked %d sessions\n", n)
			return
		}
		sessions, err := st.ListSessions(false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list: %v\n", err)
			os.Exit(1)
		}
		for _, sess := range sessions {
			state := "active"
			if sess.Revoked {
				state = "revoked"
			}
			fmt.Printf("%s  %s  %s  %s  %s\n", sess.ID[:12]+"…", time.Unix(sess.LastSeen, 0).Format(time.RFC3339), sess.IP, state, sess.UA)
		}

	default:
		adminUsage()
		os.Exit(2)
	}
}

func adminUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  fobe-server admin unblock <ip|all>    # remove IP (or all) from the login blacklist
  fobe-server admin reset-password      # new one-time password, revokes sessions
  fobe-server admin list-sessions [--revoke]`)
}
