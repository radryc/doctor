package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"

	"github.com/rydzu/ainfra/doctor/internal/federation"
	"sigs.k8s.io/yaml"
)

func main() {
	var (
		input = flag.String("input", "", "build input YAML/JSON (partitions, pushers, meta)")
		out   = flag.String("out", "", "output snapshot JSON (default stdout)")
	)
	flag.Parse()
	if strings.TrimSpace(*input) == "" {
		log.Fatal("--input is required")
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		log.Fatalf("read input: %v", err)
	}
	var buildInput federation.BuildInput
	if err := yaml.Unmarshal(raw, &buildInput); err != nil {
		log.Fatalf("parse input: %v", err)
	}
	result, err := federation.BuildCatalog(buildInput)
	if err != nil {
		log.Fatalf("build catalog: %v", err)
	}
	for _, warning := range result.Warnings {
		log.Printf("warning: %s", warning)
	}
	data, err := json.MarshalIndent(result.Snapshot, "", "  ")
	if err != nil {
		log.Fatalf("encode snapshot: %v", err)
	}
	if strings.TrimSpace(*out) == "" {
		_, _ = os.Stdout.Write(data)
		_, _ = os.Stdout.Write([]byte("\n"))
		return
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		log.Fatalf("write snapshot: %v", err)
	}
	log.Printf("wrote %d targets to %s", len(result.Snapshot.Targets), *out)
}
