// fobe-agent: the probe-side daemon (design.md §5).
//
//	fobe-agent -register -token <REGTOKEN> -server https://panel.example.com
//	fobe-agent -run [-config /etc/fobe-agent/config.json]
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/fobe-panel/fobe/internal/agent"
)

func main() {
	configPath := flag.String("config", agent.DefaultConfigPath, "config file path")
	register := flag.Bool("register", false, "register with the panel using -token")
	token := flag.String("token", "", "registration token (single use, 30 min TTL)")
	server := flag.String("server", "", "panel base URL, e.g. https://panel.example.com")
	run := flag.Bool("run", false, "run the agent against the configured server")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	switch {
	case *register:
		if *token == "" || *server == "" {
			fmt.Fprintln(os.Stderr, "fobe-agent: -register requires -token and -server")
			os.Exit(2)
		}
		cfg := &agent.Config{ServerURL: *server}
		// keep prior credentials if any: enables machine rebind (§4.2)
		if old, err := agent.LoadConfig(*configPath); err == nil {
			cfg.NodeID, cfg.NodeSecret, cfg.Iface = old.NodeID, old.NodeSecret, old.Iface
		}
		if err := agent.Register(cfg, *configPath, *token); err != nil {
			fmt.Fprintf(os.Stderr, "fobe-agent: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("registered:", cfg.NodeID)

	case *run:
		cfg, err := agent.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fobe-agent: %v\n", err)
			os.Exit(1)
		}
		if err := agent.Run(cfg, log); err != nil {
			fmt.Fprintf(os.Stderr, "fobe-agent: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "fobe-agent %s\nusage: fobe-agent -register -token <TOKEN> -server <URL>\n       fobe-agent -run [-config PATH]\n", agent.Version)
		os.Exit(2)
	}
}
