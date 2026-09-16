package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReasoningMigrationAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Start at the previous schema, including a historical audit row.
	schema := strings.Replace(sqliteSchema, "    reasoning_tokens            INTEGER NULL,\n", "", 1)
	if schema == sqliteSchema {
		t.Fatal("legacy fixture failed to remove reasoning column")
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO request_logs(id,org_id,api_key_hash,model_requested,model_used,provider,auth_mode,prompt_tokens,completion_tokens,total_tokens,cost_usd,latency_ms,status,created_at) VALUES('old','org','key','model','model','openai','api_key',20,10,30,0,1,'ok',1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	ctx := context.Background()
	for pass := 0; pass < 2; pass++ {
		st, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 {
			for _, value := range []string{"null", "0", "7"} {
				var row RequestLog
				if err := json.Unmarshal([]byte(`{"ID":"`+value+`","OrgID":"org","APIKeyHash":"key","PromptTokens":20,"CompletionTokens":10,"TotalTokens":30,"ReasoningTokens":`+value+`}`), &row); err != nil {
					t.Fatal(err)
				}
				if err := st.LogRequest(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, id := range []string{"old", "null", "0", "7"} {
			var n sql.NullInt64
			if err := st.db.QueryRow(`SELECT reasoning_tokens FROM request_logs WHERE id=?`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if id == "old" || id == "null" {
				if n.Valid {
					t.Fatalf("%s: invented %d", id, n.Int64)
				}
			} else if !n.Valid || (id == "7" && n.Int64 != 7) || (id == "0" && n.Int64 != 0) {
				t.Fatalf("%s: %v", id, n)
			}
			row, err := st.GetRequestLog(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(row)
			var decoded map[string]any
			_ = json.Unmarshal(encoded, &decoded)
			want := any(nil)
			if n.Valid {
				want = float64(n.Int64)
			}
			if got, ok := decoded["ReasoningTokens"]; !ok || got != want {
				t.Fatalf("roundtrip %s: %s", id, encoded)
			}
			if row.TotalTokens != 30 || row.CompletionTokens != 10 {
				t.Fatalf("counts: %+v", row)
			}
		}
		for _, tc := range []struct {
			key  string
			list func(context.Context, string, time.Time, int) ([]RequestLog, error)
		}{{"org", st.ListByOrg}, {"key", st.ListByAPIKey}} {
			rows, err := tc.list(ctx, tc.key, time.Time{}, 10)
			if err != nil || len(rows) != 4 {
				t.Fatalf("list: %v %v", rows, err)
			}
			for _, row := range rows {
				switch row.ID {
				case "old", "null":
					if row.ReasoningTokens != nil {
						t.Fatalf("unknown: %+v", row)
					}
				case "0":
					if row.ReasoningTokens == nil || *row.ReasoningTokens != 0 {
						t.Fatalf("zero: %+v", row)
					}
				case "7":
					if row.ReasoningTokens == nil || *row.ReasoningTokens != 7 {
						t.Fatalf("positive: %+v", row)
					}
				}
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
