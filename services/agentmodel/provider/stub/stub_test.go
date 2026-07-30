package stub_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
)

func TestStub_DefaultComplete(t *testing.T) {
	s := &stub.Stub{}
	resp, err := s.Complete(context.Background(), agentmodel.ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "test" {
		t.Errorf("Model: got %q, want test", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("Choices: got %d, want 1", len(resp.Choices))
	}
	if resp.Usage.TotalTokens == 0 {
		t.Errorf("expected non-zero usage")
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount: got %d, want 1", s.CallCount())
	}
}

func TestStub_CompleteError(t *testing.T) {
	wantErr := errors.New("intentional")
	s := &stub.Stub{CompleteErr: wantErr}
	_, err := s.Complete(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if !errors.Is(err, wantErr) {
		t.Errorf("error: got %v, want %v", err, wantErr)
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount even on error: got %d, want 1", s.CallCount())
	}
}

func TestStub_DefaultStream(t *testing.T) {
	s := &stub.Stub{}
	seq, err := s.Stream(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var chunks []provider.StreamChunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("chunk error: %v", err)
		}
		chunks = append(chunks, c)
	}
	if len(chunks) != 3 {
		t.Errorf("chunks: got %d, want 3", len(chunks))
	}
	if chunks[len(chunks)-1].FinishReason != "stop" {
		t.Errorf("last chunk FinishReason: got %q, want stop", chunks[len(chunks)-1].FinishReason)
	}
	if chunks[len(chunks)-1].Usage == nil {
		t.Errorf("last chunk should have Usage")
	}
}

func TestStub_DefaultEmbed(t *testing.T) {
	s := &stub.Stub{}
	resp, err := s.Embed(context.Background(), agentmodel.EmbeddingRequest{Model: "x", Input: []string{"hi"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Errorf("expected non-empty embeddings")
	}
}

func TestStub_AuthMode(t *testing.T) {
	s := &stub.Stub{AuthModeValue: agentmodel.AuthModeSubscription}
	if s.AuthMode() != agentmodel.AuthModeSubscription {
		t.Errorf("AuthMode: got %q, want subscription", s.AuthMode())
	}
}

func TestStub_AuthModeDefault(t *testing.T) {
	// Zero-value Stub (AuthModeValue == "") must fall back to the API-key mode.
	s := &stub.Stub{}
	if got := s.AuthMode(); got != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode default: got %q, want %q", got, agentmodel.AuthModeAPIKey)
	}
}

func TestStub_Name(t *testing.T) {
	tests := []struct {
		name      string
		nameValue string
		want      string
	}{
		{name: "default empty falls back to stub", nameValue: "", want: "stub"},
		{name: "custom name is returned verbatim", nameValue: "chatgpt", want: "chatgpt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stub.Stub{NameValue: tt.nameValue}
			if got := s.Name(); got != tt.want {
				t.Errorf("Name(): got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStub_SupportedModels(t *testing.T) {
	t.Run("default returns single stub model", func(t *testing.T) {
		s := &stub.Stub{}
		got := s.SupportedModels()
		if len(got) != 1 || got[0] != "stub-model" {
			t.Errorf("SupportedModels default: got %v, want [stub-model]", got)
		}
	})
	t.Run("custom models are returned as-is", func(t *testing.T) {
		want := []string{"gpt-4o", "gpt-4o-mini"}
		s := &stub.Stub{Models: want}
		got := s.SupportedModels()
		if len(got) != len(want) {
			t.Fatalf("SupportedModels custom len: got %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("SupportedModels[%d]: got %q, want %q", i, got[i], want[i])
			}
		}
	})
	t.Run("empty non-nil slice is preserved (not replaced by default)", func(t *testing.T) {
		s := &stub.Stub{Models: []string{}}
		got := s.SupportedModels()
		if got == nil || len(got) != 0 {
			t.Errorf("SupportedModels empty: got %v, want empty non-nil", got)
		}
	})
}

func TestStub_ListModels(t *testing.T) {
	want := []string{"gpt-4o", "gpt-5"}
	s := &stub.Stub{LiveModels: want}
	got, err := s.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListModels len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListModels[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
	if s.ListModelsCalls() != 1 {
		t.Errorf("ListModelsCalls: got %d, want 1", s.ListModelsCalls())
	}
}

func TestStub_ListModelsError(t *testing.T) {
	wantErr := errors.New("model list unavailable")
	s := &stub.Stub{ListModelsErr: wantErr}
	_, err := s.ListModels(context.Background())
	if !errors.Is(err, wantErr) {
		t.Errorf("ListModels err: got %v, want %v", err, wantErr)
	}
	if s.ListModelsCalls() != 1 {
		t.Errorf("ListModelsCalls on error: got %d, want 1", s.ListModelsCalls())
	}
}

func TestStub_StreamError(t *testing.T) {
	wantErr := errors.New("stream boom")
	s := &stub.Stub{StreamErr: wantErr}
	seq, err := s.Stream(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if !errors.Is(err, wantErr) {
		t.Errorf("Stream err: got %v, want %v", err, wantErr)
	}
	if seq != nil {
		t.Errorf("seq should be nil when Stream errors, got %v", seq)
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount even on stream error: got %d, want 1", s.CallCount())
	}
}

func TestStub_StreamCustomChunks(t *testing.T) {
	custom := []provider.StreamChunk{
		{Delta: agentmodel.Message{Role: "assistant", Content: "hello "}},
		{Delta: agentmodel.Message{Content: "world"}, FinishReason: "stop"},
	}
	s := &stub.Stub{StreamChunks: custom}
	seq, err := s.Stream(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []provider.StreamChunk
	for c, cerr := range seq {
		if cerr != nil {
			t.Fatalf("chunk error: %v", cerr)
		}
		got = append(got, c)
	}
	if len(got) != 2 {
		t.Fatalf("custom chunks: got %d, want 2", len(got))
	}
	if got[0].Delta.Content != "hello " || got[1].Delta.Content != "world" {
		t.Errorf("unexpected chunk content: %q / %q", got[0].Delta.Content, got[1].Delta.Content)
	}
	if got[1].FinishReason != "stop" {
		t.Errorf("last FinishReason: got %q, want stop", got[1].FinishReason)
	}
}

func TestStub_StreamWithDelay(t *testing.T) {
	custom := []provider.StreamChunk{
		{Delta: agentmodel.Message{Content: "a"}},
		{Delta: agentmodel.Message{Content: "b"}},
	}
	s := &stub.Stub{StreamChunks: custom, StreamDelay: time.Millisecond}
	seq, err := s.Stream(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []provider.StreamChunk
	for c, cerr := range seq {
		if cerr != nil {
			t.Fatalf("chunk error: %v", cerr)
		}
		got = append(got, c)
	}
	if len(got) != 2 {
		t.Errorf("delayed chunks: got %d, want 2", len(got))
	}
}

func TestStub_StreamCtxCancelledDuringDelay(t *testing.T) {
	// With a delay and an already-cancelled context, the select must take the
	// ctx.Done() branch and yield ctx.Err() instead of the first chunk.
	s := &stub.Stub{
		StreamChunks: []provider.StreamChunk{{Delta: agentmodel.Message{Content: "never"}}},
		StreamDelay:  50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before iterating

	seq, err := s.Stream(ctx, agentmodel.ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var sawErr error
	var chunkCount int
	for c, cerr := range seq {
		if cerr != nil {
			sawErr = cerr
			break
		}
		_ = c
		chunkCount++
	}
	if !errors.Is(sawErr, context.Canceled) {
		t.Errorf("expected context.Canceled from delay branch, got %v", sawErr)
	}
	if chunkCount != 0 {
		t.Errorf("expected no data chunks when ctx cancelled, got %d", chunkCount)
	}
}

func TestStub_StreamYieldErrAfterFirstChunk(t *testing.T) {
	// StreamYieldErr is yielded as a second item right after chunk index 0.
	yieldErr := errors.New("mid-stream failure")
	s := &stub.Stub{
		StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Content: "first"}},
			{Delta: agentmodel.Message{Content: "second"}},
		},
		StreamYieldErr: yieldErr,
	}
	seq, err := s.Stream(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var dataChunks []provider.StreamChunk
	var errs []error
	for c, cerr := range seq {
		if cerr != nil {
			errs = append(errs, cerr)
			continue
		}
		dataChunks = append(dataChunks, c)
	}
	// First data chunk, then yielded error, then second data chunk.
	if len(errs) != 1 || !errors.Is(errs[0], yieldErr) {
		t.Errorf("expected exactly one yielded error %v, got %v", yieldErr, errs)
	}
	if len(dataChunks) != 2 {
		t.Errorf("expected 2 data chunks, got %d", len(dataChunks))
	}
	if len(dataChunks) > 0 && dataChunks[0].Delta.Content != "first" {
		t.Errorf("error should follow chunk index 0; first chunk got %q", dataChunks[0].Delta.Content)
	}
}

func TestStub_StreamEarlyBreak(t *testing.T) {
	// Breaking out of the range loop must hit the !yield(...) return path
	// without panicking and without delivering the remaining chunks.
	s := &stub.Stub{StreamChunks: []provider.StreamChunk{
		{Delta: agentmodel.Message{Content: "1"}},
		{Delta: agentmodel.Message{Content: "2"}},
		{Delta: agentmodel.Message{Content: "3"}},
	}}
	seq, err := s.Stream(context.Background(), agentmodel.ChatRequest{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var seen int
	for c, cerr := range seq {
		if cerr != nil {
			t.Fatalf("unexpected error: %v", cerr)
		}
		seen++
		if c.Delta.Content == "1" {
			break // exercise the consumer-stop (!yield) return
		}
	}
	if seen != 1 {
		t.Errorf("expected to consume exactly 1 chunk before break, got %d", seen)
	}
}

func TestStub_EmbedError(t *testing.T) {
	wantErr := errors.New("embed boom")
	s := &stub.Stub{EmbedErr: wantErr}
	_, err := s.Embed(context.Background(), agentmodel.EmbeddingRequest{Model: "x", Input: []string{"hi"}})
	if !errors.Is(err, wantErr) {
		t.Errorf("Embed err: got %v, want %v", err, wantErr)
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount even on embed error: got %d, want 1", s.CallCount())
	}
}

func TestStub_EmbedCustomResponse(t *testing.T) {
	custom := agentmodel.EmbeddingResponse{
		Object: "list",
		Model:  "my-embed",
		Data: []agentmodel.Embedding{
			{Object: "embedding", Index: 0, Embedding: []float64{9, 8, 7}},
		},
		Usage: agentmodel.Usage{PromptTokens: 1, TotalTokens: 1},
	}
	s := &stub.Stub{EmbedResp: custom}
	got, err := s.Embed(context.Background(), agentmodel.EmbeddingRequest{Model: "x", Input: []string{"hi"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got.Model != "my-embed" {
		t.Errorf("custom Embed model: got %q, want my-embed", got.Model)
	}
	if len(got.Data) != 1 || len(got.Data[0].Embedding) != 3 || got.Data[0].Embedding[0] != 9 {
		t.Errorf("custom Embed data not returned verbatim: %+v", got.Data)
	}
}

func TestStub_DefaultGenerateImage(t *testing.T) {
	s := &stub.Stub{}
	resp, err := s.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "image-model", Prompt: "draw"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if resp.Model != "image-model" {
		t.Errorf("Model: got %q, want image-model", resp.Model)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON == "" {
		t.Fatalf("expected one inline image, got %+v", resp.Data)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Error("expected non-zero image usage")
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount: got %d, want 1", s.CallCount())
	}
}

func TestStub_GenerateImageCustomResponse(t *testing.T) {
	custom := agentmodel.ImageResponse{
		Model: "custom-image",
		Data:  []agentmodel.ImageData{{URL: "https://example.test/image.png"}},
		Usage: agentmodel.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
	}
	s := &stub.Stub{ImageResp: custom}
	got, err := s.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "ignored", Prompt: "draw"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if got.Model != "custom-image" || len(got.Data) != 1 || got.Data[0].URL != "https://example.test/image.png" {
		t.Errorf("custom image response not returned verbatim: %+v", got)
	}
}

func TestStub_GenerateImageError(t *testing.T) {
	wantErr := errors.New("image boom")
	s := &stub.Stub{ImageErr: wantErr}
	_, err := s.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "x", Prompt: "draw"})
	if !errors.Is(err, wantErr) {
		t.Errorf("GenerateImage err: got %v, want %v", err, wantErr)
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount even on image error: got %d, want 1", s.CallCount())
	}
}

func TestStub_MessagesPassthroughNilFn(t *testing.T) {
	// With no MessagesPassthroughFn configured, both return values are nil.
	s := &stub.Stub{}
	resp, err := s.MessagesPassthrough(context.Background(), []byte("{}"), "", "")
	if err != nil {
		t.Errorf("nil-fn passthrough err: got %v, want nil", err)
	}
	if resp != nil {
		t.Errorf("nil-fn passthrough resp: got %v, want nil", resp)
	}
}

func TestStub_MessagesPassthroughWithFn(t *testing.T) {
	var gotBody []byte
	var gotModel, gotBetas string
	s := &stub.Stub{
		MessagesPassthroughFn: func(_ context.Context, body []byte, modelOverride, clientBetas string) (*http.Response, error) {
			gotBody = body
			gotModel = modelOverride
			gotBetas = clientBetas
			return &http.Response{StatusCode: http.StatusTeapot}, nil
		},
	}
	resp, err := s.MessagesPassthrough(context.Background(), []byte(`{"x":1}`), "claude-3", "beta-1")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusTeapot {
		t.Fatalf("passthrough resp: %+v", resp)
	}
	if string(gotBody) != `{"x":1}` || gotModel != "claude-3" || gotBetas != "beta-1" {
		t.Errorf("fn received wrong args: body=%q model=%q betas=%q", gotBody, gotModel, gotBetas)
	}
}

func TestStub_PassthroughFunc(t *testing.T) {
	fn := stub.PassthroughFunc(http.StatusAccepted, "hello-body")
	resp, err := fn(context.Background(), []byte("ignored"), "", "")
	if err != nil {
		t.Fatalf("PassthroughFunc: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("StatusCode: got %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	defer resp.Body.Close()
	if string(b) != "hello-body" {
		t.Errorf("body: got %q, want hello-body", string(b))
	}
}
