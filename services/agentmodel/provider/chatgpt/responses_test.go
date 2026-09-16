package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponsesPassthrough_RewritesOnlyModel(t *testing.T) {
	original := []byte(`{"model":"logical","stream":true,"input":"weather","tools":[{"type":"web_search"}],"metadata":{"trace":"keep"}}`)
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.ResponsesPassthrough(context.Background(), original, "gpt-deployment")
	if err != nil {
		t.Fatalf("ResponsesPassthrough: %v", err)
	}
	resp.Body.Close()

	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	if string(after["model"]) != `"gpt-deployment"` {
		t.Fatalf("model = %s, want deployment id", after["model"])
	}
	delete(before, "model")
	delete(after, "model")
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("fields other than model changed\nbefore: %s\nafter:  %s", beforeJSON, afterJSON)
	}
}
