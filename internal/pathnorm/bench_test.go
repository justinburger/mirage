package pathnorm

import (
	"fmt"
	"testing"
)

var benchSink string

var benchPaths = []struct{ name, path string }{
	{"root", "/"},
	{"literal", "/api/v1/users/me/orders"},
	{"one_id", "/api/v1/users/42"},
	{"ids", "/api/v1/users/42/orders/9001/items/7"},
	{"uuid", "/api/v1/orders/123e4567-e89b-12d3-a456-426614174000"},
	{"token", "/session/abcdefghij1234567890klmn/refresh"},
	{"email", "/api/v1/users/jane.doe@example.com/prefs"},
	{"trailing_slash_query", "/api/v1/users/42/?expand=orders&limit=50"},
	{"deep", "/a/b/c/d/e/f/g/h/42/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z"},
	// Added with hash tokens and escaped-path classification.
	{"hash_token", "/api/v1/users/h_3f9a1c0b7d2e4a51/orders"},
	{"escaped_email", "/api/v1/users/jane.doe%40example.com/prefs"},
	{"escaped_literal", "/files/a%20b/report%20final.txt"},
}

func BenchmarkTemplate(b *testing.B) {
	for _, bp := range benchPaths {
		b.Run(bp.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchSink = Default.Template(bp.path)
			}
		})
	}
}

func BenchmarkTemplateParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var s string
		for pb.Next() {
			s = Default.Template("/api/v1/users/42/orders/9001")
		}
		_ = s
	})
}

func BenchmarkEndpoint(b *testing.B) {
	for _, bp := range benchPaths {
		b.Run(bp.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchSink = Default.Endpoint("get", bp.path)
			}
		})
	}
}

func BenchmarkTemplateWithPatterns(b *testing.B) {
	for _, count := range []int{1, 10, 50} {
		patterns := make([]string, 0, count)
		for i := range count - 1 {
			patterns = append(patterns, fmt.Sprintf("/svc%d/{id}/items/{item}", i))
		}
		patterns = append(patterns, "/api/v1/users/{id}/orders/{oid}")
		n, err := New(patterns)
		if err != nil {
			b.Fatal(err)
		}
		for _, bp := range []struct{ name, path string }{
			{"hit", "/api/v1/users/jane/orders/o-1"},
			{"miss_heuristic", "/api/v1/accounts/42/orders/9001"},
		} {
			b.Run(fmt.Sprintf("%s/patterns=%d", bp.name, count), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink = n.Template(bp.path)
				}
			})
		}
	}
}

func BenchmarkClassify(b *testing.B) {
	for _, seg := range []string{
		"users", "42", "123e4567-e89b-12d3-a456-426614174000", "abcdefghij1234567890", "jane@example.com", "2024-01-31",
		"h_3f9a1c0b7d2e4a51", "jane%40example.com", "%31%32%33",
	} {
		b.Run(seg, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchSink = classify(seg)
			}
		})
	}
}

func BenchmarkNew(b *testing.B) {
	patterns := []string{"/users/{id}", "/users/{id}/orders/{oid}", "/files/{path...}", "/health", "/v1/{a}/{b}"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(patterns); err != nil {
			b.Fatal(err)
		}
	}
}
