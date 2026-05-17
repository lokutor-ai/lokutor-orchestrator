// Package vela provides turn detection using a lightweight neural model.
// It replaces the traditional VAD-based turn detection with a model that
// predicts floor_yield, continuation_confidence, and interruption_safety.
package vela

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	SampleRate     = 16000
	FrameDuration  = 0.02
	FrameSize      = int(SampleRate * FrameDuration) // 320 samples
	FeatureDim     = 10
	VADThreshold   = 0.02
	VADHysteresis  = 0.005
)

// TurnEvent represents the output of the Vela turn detector.
type TurnEvent struct {
	FloorYield         float32 // Probability that the user is yielding the floor
	Continuation       float32 // Confidence that the user will continue speaking
	InterruptionSafety float32 // How safe it is for the bot to interrupt
}

// Detector is a Vela turn detection model loaded from ONNX.
type Detector struct {
	mu      sync.Mutex
	session *ort.AdvancedSession

	// Input/output tensors
	input  *ort.Tensor[float32]
	output *ort.Tensor[float32]

	// Feature state (maintained across frames)
	vadState        bool
	silenceDuration float32
	speechDuration  float32
	prevRMS         float32
	voicedRatio     float32

	// VAD state
	vadThreshold  float32
	vadHysteresis float32
	speechCount   int
	silenceCount  int
	minSpeechFrames int
	minSilenceFrames int
}

// NewDetector loads the Vela ONNX model.
func NewDetector(modelPath string) (*Detector, error) {
	libPath := os.Getenv("ONNXRUNTIME_LIB_PATH")
	if libPath == "" {
		if runtime.GOOS == "darwin" {
			libPath = "/opt/homebrew/lib/libonnxruntime.dylib"
		} else {
			libPath = "/usr/local/lib/libonnxruntime.so"
		}
	}
	ort.SetSharedLibraryPath(libPath)

	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("init onnx: %w", err)
	}

	input, err := ort.NewEmptyTensor[float32]([]int64{1, FeatureDim})
	if err != nil {
		return nil, fmt.Errorf("create input tensor: %w", err)
	}

	output, err := ort.NewEmptyTensor[float32]([]int64{1, 3})
	if err != nil {
		return nil, fmt.Errorf("create output tensor: %w", err)
	}

	session, err := ort.NewAdvancedSession(
		modelPath,
		[]string{"features"},
		[]string{"predictions"},
		[]ort.ArbitraryTensor{input},
		[]ort.ArbitraryTensor{output},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("load onnx model: %w", err)
	}

	return &Detector{
		session:          session,
		input:            input,
		output:           output,
		vadThreshold:     VADThreshold,
		vadHysteresis:    VADHysteresis,
		minSpeechFrames:  3,
		minSilenceFrames: 5,
		voicedRatio:      0.5,
	}, nil
}

// Process runs VAD + feature extraction + model inference on a single audio chunk.
// The chunk should be 320 samples (20ms) of int16 PCM at 16kHz.
func (d *Detector) Process(chunk []byte) (*TurnEvent, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Convert int16 bytes to float32 samples
	samples := make([]float32, len(chunk)/2)
	for i := 0; i < len(samples); i++ {
		// Little-endian int16
		raw := int16(chunk[i*2]) | int16(chunk[i*2+1])<<8
		samples[i] = float32(raw) / 32768.0
	}

	// Compute RMS
	var sumSq float32
	for _, s := range samples {
		sumSq += s * s
	}
	rms := float32(math.Sqrt(float64(sumSq / float32(len(samples)))))

	// VAD decision with hysteresis
	vadActive := d.updateVAD(rms)

	// Update duration trackers
	if vadActive {
		d.speechDuration += FrameDuration
		d.silenceDuration = 0
	} else {
		d.silenceDuration += FrameDuration
		d.speechDuration = 0
	}

	// Energy slope
	energySlope := (rms - d.prevRMS) / FrameDuration
	d.prevRMS = rms

	// Build feature vector (matches Python training pipeline)
	features := [FeatureDim]float32{
		boolToFloat(vadActive),                          // 0: vad_state
		clamp(d.silenceDuration/2.0, 1.0),              // 1: silence_duration
		clamp(d.speechDuration/5.0, 1.0),               // 2: speech_duration
		clamp(rms/0.1, 1.0),                            // 3: rms_energy
		clamp(energySlope/100.0, 1.0),                  // 4: energy_slope (clamped to [-1, 1] below)
		d.voicedRatio,                                   // 5: voiced_ratio
		0.0,                                            // 6: pitch_slope (placeholder)
		d.voicedRatio,                                   // 7: voiced_ratio_dup
		clamp(d.voicedRatio, 1.0),                      // 8: speaking_rate proxy
		0.0,                                            // 9: interruption_count (placeholder)
	}

	// Clamp energy slope to [-1, 1]
	if features[4] > 1.0 {
		features[4] = 1.0
	} else if features[4] < -1.0 {
		features[4] = -1.0
	}

	// Copy features to input tensor
	copy(d.input.GetData(), features[:])

	// Run inference
	if err := d.session.Run(); err != nil {
		return nil, fmt.Errorf("inference: %w", err)
	}

	// Read outputs
	outData := d.output.GetData()
	event := &TurnEvent{
		FloorYield:         outData[0],
		Continuation:       outData[1],
		InterruptionSafety: outData[2],
	}

	return event, nil
}

// updateVAD implements the same energy-based VAD with hysteresis as the Python version.
func (d *Detector) updateVAD(rms float32) bool {
	if d.vadState {
		// Currently speech — check for silence
		if rms < (d.vadThreshold - d.vadHysteresis) {
			d.silenceCount++
			if d.silenceCount >= d.minSilenceFrames {
				d.vadState = false
				d.silenceCount = 0
			}
		} else {
			d.silenceCount = 0
		}
	} else {
		// Currently silence — check for speech
		if rms > (d.vadThreshold + d.vadHysteresis) {
			d.speechCount++
			if d.speechCount >= d.minSpeechFrames {
				d.vadState = true
				d.speechCount = 0
			}
		} else {
			d.speechCount = 0
		}
	}
	return d.vadState
}

// IsSpeaking returns whether VAD currently detects speech.
func (d *Detector) IsSpeaking() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.vadState
}

// Reset clears all state.
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.vadState = false
	d.silenceDuration = 0
	d.speechDuration = 0
	d.prevRMS = 0
	d.speechCount = 0
	d.silenceCount = 0
}

// Destroy cleans up ONNX resources.
func (d *Detector) Destroy() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		d.session.Destroy()
	}
	if d.input != nil {
		d.input.Destroy()
	}
	if d.output != nil {
		d.output.Destroy()
	}
}

func clamp(v, max float32) float32 {
	if v > max {
		return max
	}
	if v < 0 {
		return 0
	}
	return v
}

func boolToFloat(b bool) float32 {
	if b {
		return 1.0
	}
	return 0.0
}
