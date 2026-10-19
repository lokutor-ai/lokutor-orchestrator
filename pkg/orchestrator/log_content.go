package orchestrator

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// What callers and agents say (transcripts, partials, replies, prompts, tool arguments) does not go
// into log lines: logs are shipped to CloudWatch and readable by anyone with cluster access, and the
// conversation is already stored where it belongs (conversation_history). A log line keeps the
// operational part instead: the text's length in characters, its word count and a short keyed hash,
// so two lines about the same text can still be matched (the key is random per process, so the hash
// correlates lines from one pod and cannot be looked up against a list of names or numbers).
//
// LOG_CALL_CONTENT=1 puts the text back, for local debugging. It is read here and nowhere else, once
// per process; production manifests must not set it.

const logCallContentEnv = "LOG_CALL_CONTENT"

var (
	logCallContentOnce sync.Once
	logCallContent     atomic.Bool
	logHashKey         []byte
)

func initLogCallContent() {
	logCallContentOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(logCallContentEnv))) {
		case "1", "true", "yes", "on":
			logCallContent.Store(true)
		}
		logHashKey = make([]byte, 32)
		if _, err := rand.Read(logHashKey); err != nil {
			logHashKey = nil // ContentForLog then omits the hash
		}
	})
}

// LogCallContent reports whether log lines may carry call content (LOG_CALL_CONTENT=1). Off by
// default. Exported so the host process gates its own lines on the same switch.
func LogCallContent() bool {
	initLogCallContent()
	return logCallContent.Load()
}

// SetLogCallContent overrides LOG_CALL_CONTENT for this process (tests).
func SetLogCallContent(on bool) {
	initLogCallContent()
	logCallContent.Store(on)
}

// ContentForLog is what a log line carries in place of call content: the text itself (clipped to
// 400 characters) when LOG_CALL_CONTENT is on, otherwise "[N chars, W words, h:xxxxxxxx]". An empty
// string stays empty, so an empty transcript still reads as one.
func ContentForLog(s string) string {
	if s == "" {
		return ""
	}
	if LogCallContent() {
		return clipForLog(s)
	}
	chars := utf8.RuneCountInString(s)
	words := len(strings.Fields(s))
	if logHashKey == nil {
		return fmt.Sprintf("[%d chars, %d words]", chars, words)
	}
	m := hmac.New(sha256.New, logHashKey)
	m.Write([]byte(s))
	return fmt.Sprintf("[%d chars, %d words, h:%s]", chars, words, hex.EncodeToString(m.Sum(nil)[:4]))
}
