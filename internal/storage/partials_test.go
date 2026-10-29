package storage

import (
	"context"
	"slices"
	"testing"
)

// Partial downloads are recorded per account; Take lists and clears them.
func TestPartials(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	a, b := NewPartials(db, "alice"), NewPartials(db, "bob")

	for _, p := range []string{"/r/.telegram-mcp-1.part", "/r/.telegram-mcp-2.part"} {
		if err := a.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Add(ctx, "/r/.telegram-mcp-1.part"); err != nil {
		t.Fatalf("adding a path twice: %v", err)
	}
	if err := b.Add(ctx, "/r/.telegram-mcp-3.part"); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(ctx, "/r/.telegram-mcp-2.part"); err != nil {
		t.Fatal(err)
	}

	got, err := a.Take(ctx)
	if err != nil || !slices.Equal(got, []string{"/r/.telegram-mcp-1.part"}) {
		t.Fatalf("Take = %v, %v", got, err)
	}
	if again, _ := a.Take(ctx); len(again) != 0 {
		t.Errorf("second Take = %v, want empty", again)
	}
	if other, _ := b.Take(ctx); !slices.Equal(other, []string{"/r/.telegram-mcp-3.part"}) {
		t.Errorf("bob's partials = %v", other)
	}
}
