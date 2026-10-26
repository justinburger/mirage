package config

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/justinburger/mirage/internal/store"
)

func exampleBytes(b *testing.B) []byte {
	b.Helper()
	data, err := os.ReadFile("../../examples/mirage.yaml")
	if err != nil {
		b.Fatal(err)
	}
	return data
}

func BenchmarkParseYAML(b *testing.B) {
	data := exampleBytes(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := Parse("mirage.yaml", data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseJSON(b *testing.B) {
	data := []byte(fullJSON)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := Parse("mirage.json", data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExpandEnv(b *testing.B) {
	data := []byte("server:\n  admin_token: ${TOKEN}\n  data: ${DATA:-/var/lib/mirage.db}\n  # ${IGNORED}\nservices:\n  - listen_addr: \":${PORT}\"\n")
	env := envMap(map[string]string{"TOKEN": "tok", "PORT": "9001"})
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := expandEnv(data, true, env); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExpandEnvNoReferences(b *testing.B) {
	data := bytes.ReplaceAll(exampleBytes(b), []byte("${"), []byte("$ {"))
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := expandEnv(data, true, os.LookupEnv); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyUnchanged measures start-up cost when the file is already
// applied, which is every start after the first.
func BenchmarkApplyUnchanged(b *testing.B) {
	c, err := Load("../../examples/mirage.yaml")
	if err != nil {
		b.Fatal(err)
	}
	st := openStore(b)
	ctx := context.Background()
	if _, err := c.Apply(ctx, st); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := c.Apply(ctx, st)
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Created)+len(res.Updated) != 0 {
			b.Fatalf("not idempotent: %+v", res)
		}
	}
}

// BenchmarkApplyFresh measures applying the example to an empty database.
func BenchmarkApplyFresh(b *testing.B) {
	c, err := Load("../../examples/mirage.yaml")
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		st, err := store.Open(store.MemoryPath)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := c.Apply(ctx, st); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		st.Close()
		b.StartTimer()
	}
}
