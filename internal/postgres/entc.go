//go:build ignore

package main

import (
	"log"
	"path/filepath"
	"runtime"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

func main() {
	config := &gen.Config{
		Features: []gen.Feature{
			gen.FeatureUpsert,
		},
	}
	if err := entc.Generate(filepath.Join(currentFileDir(), "schema"), config); err != nil {
		log.Fatalf("ent codegen failed: %v", err)
	}
}

func currentFileDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		log.Fatal("failed to get current file path")
	}
	return filepath.Dir(file)
}
