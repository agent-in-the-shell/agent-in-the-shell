package api

import (
	"math"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
)

func costServer(t *testing.T) *Server {
	t.Helper()
	reg, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return &Server{registry: reg}
}

func TestReplicatePredictionCost(t *testing.T) {
	s := costServer(t)
	cases := []struct {
		name, model, body string
		wantCost          float64
		wantSrc           cost.Source
	}{
		// Kling: per output-video-second, tiered by (mode, generate_audio); duration default 5.
		{"kling default tier (standard/noaudio/5s)", "kwaivgi/kling-v3-omni-video", `{"input":{"prompt":"x"}}`, 0.168 * 5, cost.SourcePriced},
		{"kling pro+audio 10s", "kwaivgi/kling-v3-omni-video", `{"input":{"mode":"pro","generate_audio":true,"duration":10}}`, 0.28 * 10, cost.SourcePriced},
		{"kling standard+audio 5s", "kwaivgi/kling-v3-omni-video", `{"input":{"generate_audio":true}}`, 0.224 * 5, cost.SourcePriced},
		{"kling 4k (audio irrelevant) 3s", "kwaivgi/kling-v3-omni-video", `{"input":{"mode":"4k","duration":3}}`, 0.42 * 3, cost.SourcePriced},
		{"kling unknown mode -> unpriced", "kwaivgi/kling-v3-omni-video", `{"input":{"mode":"ultra"}}`, 0, cost.SourceUnpriced},

		// Qwen image: flat per image, always exactly 1 output.
		{"qwen-image", "qwen/qwen-image", `{"input":{"prompt":"x"}}`, 0.025, cost.SourcePriced},
		{"qwen-image-edit-plus (ref images not counted)", "qwen/qwen-image-edit-plus", `{"input":{"prompt":"x","image":["a","b","c"]}}`, 0.03, cost.SourcePriced},

		// MiniMax TTS: per input character (runes), output $0.
		{"minimax turbo 10 ascii chars", "minimax/speech-02-turbo", `{"input":{"text":"0123456789"}}`, 0.00003 * 10, cost.SourcePriced},
		{"minimax hd 4 CJK runes", "minimax/speech-02-hd", `{"input":{"text":"你好世界"}}`, 0.00005 * 4, cost.SourcePriced},
		{"minimax 2.6-turbo empty text", "minimax/speech-2.6-turbo", `{"input":{"text":""}}`, 0, cost.SourcePriced},

		// Unpriced cases.
		{"bria stays unpriced (output len not in request)", "bria/video-remove-background", `{"input":{"video_url":"u"}}`, 0, cost.SourceUnpriced},
		{"unknown model unpriced", "acme/something", `{"input":{}}`, 0, cost.SourceUnpriced},
		{"bare version hash unpriced", "578b58hash", `{"version":"578b58hash","input":{}}`, 0, cost.SourceUnpriced},
		{"malformed body unpriced", "qwen/qwen-image", `not json`, 0.025, cost.SourcePriced}, // qwen ignores input, so still priced at 1 image
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, src := s.replicatePredictionCost(tc.model, []byte(tc.body))
			if src != tc.wantSrc {
				t.Errorf("source = %q, want %q", src, tc.wantSrc)
			}
			if math.Abs(got-tc.wantCost) > 1e-9 {
				t.Errorf("cost = %v, want %v", got, tc.wantCost)
			}
		})
	}
}

func TestReplicatePredictionCost_NilRegistry(t *testing.T) {
	s := &Server{} // registry nil
	if c, src := s.replicatePredictionCost("qwen/qwen-image", []byte(`{"input":{}}`)); c != 0 || src != cost.SourceUnpriced {
		t.Errorf("nil registry: got (%v,%q), want (0,unpriced)", c, src)
	}
}
