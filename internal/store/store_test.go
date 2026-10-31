package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentWritesAndSearch(t *testing.T) {
	st, err := Open(Config{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, 5, "test")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := st.AppendMessages(ctx, sess.ID, []MessageRecord{{Role: "user", Content: fmt.Sprintf("uniquephrase %d", i)}}); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	msgs, err := st.Messages(ctx, sess.ID, 100)
	if err != nil || len(msgs) != 20 {
		t.Fatalf("messages: %d %v", len(msgs), err)
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Fatalf("sequence at %d = %d", i, m.Seq)
		}
	}
	results, err := st.SearchMessages(ctx, "uniquephrase", 30)
	if err != nil || len(results) != 20 {
		t.Fatalf("FTS results: %d %v", len(results), err)
	}
}

func BenchmarkSQLiteAppendMessage(b *testing.B) {
	st, err := Open(Config{Path: filepath.Join(b.TempDir(), "bench.db")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := st.Close(context.Background()); err != nil {
			b.Error(err)
		}
	})
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, 5, "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	msg := []MessageRecord{{Role: "user", Content: "benchmark message"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.AppendMessages(ctx, sess.ID, msg); err != nil {
			b.Fatal(err)
		}
	}
}
