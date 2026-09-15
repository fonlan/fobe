// fobe-server: the panel server + scheduler + admin CLI (design.md §4.1, §17).
//
// Usage:
//
//	fobe-server                     # run the panel (env: FOBE_MASTER_KEY, FOBE_DB, …)
//	fobe-server admin unblock <ip|all>
//	fobe-server admin reset-password
//	fobe-server admin list-sessions [--revoke]
//	fobe-server admin kill-switch on|off
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fobe-panel/fobe/internal/server/geoip"
	"github.com/fobe-panel/fobe/internal/server/httpapi"
	"github.com/fobe-panel/fobe/internal/server/hub"
	"github.com/fobe-panel/fobe/internal/server/notify"
	"github.com/fobe-panel/fobe/internal/server/scheduler"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/singboxcache"
	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
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

func mustStore() *store.Store {
	st, err := store.Open(env("FOBE_DB", "/data/fobe.db"))
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
		fmt.Fprintln(os.Stderr, "fobe-server: FOBE_MASTER_KEY is required (AI keys, SSH credentials and bot tokens are encrypted with it).")
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
	st := mustStore()
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
	h := hub.New(st, trust, log, geoip.New(geoPath, geoOnline))
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
	// right away; the hub's chain resolver picks the file up on its next
	// lookup via its own stat-based reload. Both views share the same file.
	api.GeoIPMMDBPath = geoPath
	api.GeoIPResolver = geoip.NewMMDB(geoPath)

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
	channels := []notify.Notifier{
		notify.NewTelegram(decryptSetting, notifyClient),
		notify.NewWebhook(decryptSetting, notifyClient),
	}
	sched := scheduler.New(st, log, env("FOBE_BACKUP_DIR", ""), 7, channels...)
	sched.Start(stop)
	go h.PumpCommands(2 * time.Second)

	// Server-side sing-box artifact cache (§9.2): download the current stable
	// release when the cache is empty, record the verdict in settings for the
	// panel, and warn when the artifact directory would not survive a container
	// upgrade. It never blocks startup.
	api.SingboxCache = startSingboxCache(bgCtx, st, log, api.SingboxDL())

	// §9.2 one-click batch update: build the manager now and re-arm the
	// convergence check of a job that was still pending when the process last
	// stopped (the deadline is persisted with the job).
	api.SingboxUpdater().Start(bgCtx)

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
		st := mustStore()
		defer st.Close()
		if err := st.UnblockIP(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "unblock: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("unblocked %s\n", args[1])

	case "reset-password":
		st := mustStore()
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
		st := mustStore()
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

	case "kill-switch":
		if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
			fmt.Fprintln(os.Stderr, "usage: fobe-server admin kill-switch on|off")
			os.Exit(2)
		}
		st := mustStore()
		defer st.Close()
		val := "0"
		if args[1] == "on" {
			val = "1"
		}
		if err := st.SetSetting("ai.kill_switch", val, false); err != nil {
			fmt.Fprintf(os.Stderr, "kill-switch: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("AI kill switch: %s\n", args[1])

	default:
		adminUsage()
		os.Exit(2)
	}
}

func adminUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  fobe-server admin unblock <ip|all>    # remove IP (or all) from the login blacklist
  fobe-server admin reset-password      # new one-time password, revokes sessions
  fobe-server admin list-sessions [--revoke]
  fobe-server admin kill-switch on|off  # freeze all AI execution`)
}
