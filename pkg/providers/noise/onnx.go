package noise

import (
	"fmt"

	ort "github.com/yalue/onnxruntime_go"
)

// Suppressor wraps the ONNX model for noise suppression.
type Suppressor struct {
	session     *ort.AdvancedSession
	featuresInput  *ort.Tensor[float32]
	hiddenInput    *ort.Tensor[float32]
	gainsOutput    *ort.Tensor[float32]
	dfOutput       *ort.Tensor[float32]
	vadOutput      *ort.Tensor[float32]
	newHiddenOutput *ort.Tensor[float32]
}

// NewSuppressor loads an ONNX noise suppression model.
func NewSuppressor(modelPath string) (*Suppressor, error) {
	ort.SetSharedLibraryPath("/usr/local/lib/libonnxruntime.so")
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("failed to initialize ONNX runtime: %w", err)
	}

	// Create input tensors
	featuresInput, err := ort.NewEmptyTensor[float32]([]int64{1, int64(NFeatures)})
	if err != nil {
		return nil, err
	}

	hiddenInput, err := ort.NewEmptyTensor[float32]([]int64{int64(GRULayers), 1, int64(GRUUnits)})
	if err != nil {
		return nil, err
	}

	// Create output tensors
	gainsOutput, err := ort.NewEmptyTensor[float32]([]int64{1, int64(NBands)})
	if err != nil {
		return nil, err
	}

	dfOutput, err := ort.NewEmptyTensor[float32]([]int64{1, int64(NDFBins), 2})
	if err != nil {
		return nil, err
	}

	vadOutput, err := ort.NewEmptyTensor[float32]([]int64{1, 1})
	if err != nil {
		return nil, err
	}

	newHiddenOutput, err := ort.NewEmptyTensor[float32]([]int64{int64(GRULayers), 1, int64(GRUUnits)})
	if err != nil {
		return nil, err
	}

	session, err := ort.NewAdvancedSession(
		modelPath,
		[]string{"features", "hidden_state"},
		[]string{"gains", "df_coefs", "vad", "new_hidden_state"},
		[]ort.ArbitraryTensor{featuresInput, hiddenInput},
		[]ort.ArbitraryTensor{gainsOutput, dfOutput, vadOutput, newHiddenOutput},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load ONNX model: %w", err)
	}

	return &Suppressor{
		session:         session,
		featuresInput:   featuresInput,
		hiddenInput:     hiddenInput,
		gainsOutput:     gainsOutput,
		dfOutput:        dfOutput,
		vadOutput:       vadOutput,
		newHiddenOutput: newHiddenOutput,
	}, nil
}

// ProcessFrame runs inference on a single frame.
// features: (78,) float32
// hidden: (3*1*256 = 768,) float32
// Returns: gains (34,), new_hidden (768,)
func (s *Suppressor) ProcessFrame(features, hidden []float32) ([]float32, []float32, error) {
	// Copy input data
	copy(s.featuresInput.GetData(), features)
	copy(s.hiddenInput.GetData(), hidden)

	// Run inference
	if err := s.session.Run(); err != nil {
		return nil, nil, fmt.Errorf("inference failed: %w", err)
	}

	// Copy outputs
	gains := make([]float32, NBands)
	copy(gains, s.gainsOutput.GetData())

	newHidden := make([]float32, GRULayers*1*GRUUnits)
	copy(newHidden, s.newHiddenOutput.GetData())

	return gains, newHidden, nil
}

// Destroy cleans up the ONNX session.
func (s *Suppressor) Destroy() {
	if s.session != nil {
		s.session.Destroy()
	}
	if s.featuresInput != nil {
		s.featuresInput.Destroy()
	}
	if s.hiddenInput != nil {
		s.hiddenInput.Destroy()
	}
	if s.gainsOutput != nil {
		s.gainsOutput.Destroy()
	}
	if s.dfOutput != nil {
		s.dfOutput.Destroy()
	}
	if s.vadOutput != nil {
		s.vadOutput.Destroy()
	}
	if s.newHiddenOutput != nil {
		s.newHiddenOutput.Destroy()
	}
}
