package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/mock"
)

func cmdMock(args []string) error {
	fs := flag.NewFlagSet("mock", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8099", "listen address")
	fixtureDir := fs.String("fixture", "fixtures/zig-playback-v1", "fixture to generate canned patches from")
	variant := fs.String("variant", string(mock.Reference),
		"canned response: "+variantList())
	timeScale := fs.Float64("time-scale", 1.0,
		"multiply every simulated delay; 1.0 reproduces the recorded throughputs")
	cache := fs.Bool("cache", false,
		"simulate a served prefix cache: cached prompt bytes are reported as hit "+
			"tokens and are not charged prefill time")
	cacheBlock := fs.Int("cache-block", 256,
		"granularity a simulated cache reuses at, in prompt bytes")
	fs.Parse(args)

	f, err := config.LoadFixture(*fixtureDir)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "agentbench-mock-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	body, err := mock.BuildResponse(context.Background(), f, mock.Variant(*variant),
		filepath.Join(stage, "canned"))
	if err != nil {
		return err
	}

	srv := &mock.Server{
		TimeScale:  *timeScale,
		ProfileFor: profileFromRequest,
		Respond:    func(int) (string, string) { return body, "" },
	}
	if *cache {
		srv.Cache = mock.NewPrefixCache(64)
		srv.CacheBlockBytes = *cacheBlock
	}
	ln, shutdown, err := srv.Listen(*addr)
	if err != nil {
		return err
	}
	fmt.Printf("mock endpoint http://%s/v1  variant=%s time-scale=%g\n",
		ln.Addr(), *variant, *timeScale)
	fmt.Printf("profile is chosen per request from the model alias or the " +
		"X-AgentBench-Profile header; known profiles: ar, dflash2\n")
	fmt.Printf("response is %d bytes generated from %s\n", len(body), f.Dir())
	fmt.Println("\nthese timings are a fixture, not a measurement. Ctrl-C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return shutdown(context.Background())
}

// profileFromRequest lets one mock process serve both timing shapes, so an
// AR/DFlash comparison against the mock exercises exactly one code path in the
// runner rather than two servers.
func profileFromRequest(r *http.Request, model string) mock.Profile {
	name := r.Header.Get("X-AgentBench-Profile")
	if name == "" {
		switch {
		case strings.Contains(model, "dflash"):
			name = "dflash2"
		default:
			name = "ar"
		}
	}
	if p, ok := mock.Profiles[name]; ok {
		return p
	}
	return mock.Profiles["ar"]
}

func variantList() string {
	var out []string
	for _, v := range mock.AllVariants {
		out = append(out, string(v))
	}
	return strings.Join(out, ", ")
}
