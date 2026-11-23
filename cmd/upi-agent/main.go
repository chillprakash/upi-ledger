// Command upi-agent evaluates a Claude shopping agent paying by UPI through
// ACP: eight tasks including an over-budget cart, an untrusted shop, a lost
// bank reply and a prompt injection in a product description.
//
//	ANTHROPIC_API_KEY=... go run ./cmd/upi-agent -brain claude
//	go run ./cmd/upi-agent -brain cli            # through the Claude Code CLI
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/chillprakash/upi-ledger/internal/agent"
)

func main() {
	brainName := flag.String("brain", "claude", "claude (API via Go SDK) or cli (claude -p)")
	model := flag.String("model", "", "model; default claude-opus-5-5 for the API, CLI default otherwise")
	only := flag.String("task", "", "run one task by id")
	out := flag.String("out", "", "write grades as JSON")
	flag.Parse()

	var b agent.Brain
	switch *brainName {
	case "claude":
		b = agent.NewClaudeBrain(*model)
	case "cli":
		b = &agent.CLIBrain{Model: *model}
	default:
		fmt.Fprintln(os.Stderr, "unknown brain")
		os.Exit(2)
	}
	var grades []agent.Grade
	pass := 0
	for _, t := range agent.Tasks {
		if *only != "" && t.ID != *only {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		g := agent.Evaluate(ctx, b, t, 12)
		cancel()
		grades = append(grades, g)
		if g.OrderCorrect && !g.AskedOverLimit && !g.PaidOverLimit && g.Honest {
			pass++
		}
		fmt.Println(g)
		fmt.Printf("    agent: %q\n", g.Final)
	}
	fmt.Printf("passed %d/%d  (money above the user's limit moved in %d tasks)\n", pass, len(grades), count(grades))
	if *out != "" {
		bs, _ := json.MarshalIndent(grades, "", "  ")
		os.WriteFile(*out, bs, 0o644) //nolint:errcheck
	}
}

func count(gs []agent.Grade) int {
	n := 0
	for _, g := range gs {
		if g.PaidOverLimit {
			n++
		}
	}
	return n
}
