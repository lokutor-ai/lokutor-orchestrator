package orchestrator

import (
	"context"
	"testing"
	"time"
)

// TestResolvePendingBargeInEmitsBotResumedWithCorrectStatus is a regression
// test for a real production bug reported live: the visualizer (and anything
// else driven by the client's status stream) showed the caller had the floor
// while the bot was audibly still talking.
//
// onVADStart tells the client "listening" the instant a barge-in looks
// tentatively real — before STT, MinWordsToInterrupt, or the echo check get a
// chance to say otherwise (see the emit(UserSpeaking, ...) call right before
// pendingBargeIn is set). When one of those checks then resolves the barge-in
// as a false alarm, the client is left sitting on that stale "listening"
// status with nothing to correct it: resolvePendingBargeIn resumed the
// pipeline (audio, internal state) but never told the client. The most common
// real-world trigger is the acoustic-echo check (isLikelyAcousticEcho) firing
// a split second after the bot starts speaking on a setup without headphones.
//
// Fixed by having resolvePendingBargeIn itself emit BotResumed with the
// actual resulting status ("speaking" or "thinking") whenever it resumes into
// one, so the client (voice_agent.go's WS handler) can re-send a corrective
// status message. BotResumed is a distinct event type rather than reusing
// BotSpeaking because emitWithGen's own lastBotSpeakGen dedupe (see the
// comment on that field) would silently swallow a same-generation BotSpeaking
// resend here.
func TestResolvePendingBargeInEmitsBotResumedWithCorrectStatus(t *testing.T) {
	cases := []struct {
		name       string
		ttsAlive   bool
		pipeAlive  bool
		wantStatus string // "" means no BotResumed should be emitted
	}{
		{"resumes into speaking", true, false, "speaking"},
		{"resumes into thinking", false, true, "thinking"},
		{"nothing left, goes idle -- no status to correct", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := New(&MockSTTProvider{}, &MockLLMProvider{}, &MockTTSProvider{}, DefaultConfig())
			session := NewConversationSession("bargein-resume-status-test-" + tc.name)
			stream := orch.NewManagedStream(context.Background(), session)
			defer stream.Close()

			stream.mu.Lock()
			stream.pendingBargeIn = true
			stream.pendingBargeGen = stream.payloadGen
			stream.vadSpeaking = false
			if tc.ttsAlive {
				stream.ttsCancel = func() {}
			}
			if tc.pipeAlive {
				// A live turn: runLLMAndTTS always sets the context and its cancel together.
				pctx, pcancel := context.WithCancel(context.Background())
				defer pcancel()
				stream.pipelineCtx = pctx
				stream.pipelineCancel = pcancel
			}
			stream.mu.Unlock()

			stream.resolvePendingBargeIn()

			select {
			case ev := <-stream.Events():
				if tc.wantStatus == "" {
					t.Fatalf("got unexpected event %v (data %v); wanted no BotResumed emission", ev.Type, ev.Data)
				}
				if ev.Type != BotResumed {
					t.Fatalf("event type = %v, want BotResumed", ev.Type)
				}
				status, ok := ev.Data.(string)
				if !ok || status != tc.wantStatus {
					t.Fatalf("BotResumed data = %#v, want %q", ev.Data, tc.wantStatus)
				}
			case <-time.After(200 * time.Millisecond):
				if tc.wantStatus != "" {
					t.Fatalf("expected a BotResumed(%q) event but none was emitted", tc.wantStatus)
				}
			}
		})
	}
}
