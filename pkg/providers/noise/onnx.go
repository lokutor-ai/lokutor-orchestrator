package noise

import (
	"fmt"

	ort "github.com/yalue/onnxruntime_go"
)

// Suppressor wraps the ONNX model for noise suppression.
type Suppressor struct {
	session      *ort.AdvancedSession
	inputNames   []string
	outputNames  []string
}

// NewSuppressor loads an ONNX noise suppression model.
func NewSuppressor(modelPath string) (*Suppressor, error) {
	ort.SetSharedLibraryPath("/usr/local/lib/libonnxruntime.so")
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("failed to initialize ONNX runtime: %w", err)
	}

	inputNames := []string{"features", "hidden_state"}
	outputNames := []string{"gains", "df_coefs", "vad", "new_hidden_state"}

	session, err := ort.NewAdvancedSession(
		modelPath,
		inputNames,
		outputNames,
		[]ort.ArbitraryTensor{
			&ort.Tensor[float32]{},
			&ort.Tensor[float32]{},
		},
		[]ort.ArbitraryTensor{
			&ort.Tensor[float32]{},
			&ort.Tensor[float32]{},
			&ort.Tensor[float32]{},
			&ort.Tensor[float32]{},
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load ONNX model: %w", err)
	}

	return &Suppressor{
		session:     session,
		inputNames:  inputNames,
		outputNames: outputNames,
	}, nil
}

// ProcessFrame runs inference on a single frame.
// features: (1, 78) float32
// hidden: (3, 1, 256) float32
// Returns: gains (34,), new_hidden (3, 1, 256)
func (s *Suppressor) ProcessFrame(features, hidden []float32) ([]float32, []float32, error) {
	featuresTensor, err := ort.NewTensor([]int64{1, int64(NFeatures)}, features)
	if err != nil {
		return nil, nil, err
	}
	defer featuresTensor.Destroy()

	hiddenTensor, err := ort.NewTensor([]int64{int64(GRULayers), 1, int64(GRUUnits)}, hidden)
	if err != nil {
		return nil, nil, err
	}
	defer hiddenTensor.Destroy()

	inputs := []ort.ArbitraryTensor{featuresTensor, hiddenTensor}
	
	gainsTensor, err := ort.NewEmptyTensor[float32]([]int64{1, int64(NBands)})
	if err != nil {
		return nil, nil, err
	}
	defer gainsTensor.Destroy()

	dfTensor, err := ort.NewEmptyTensor[float32]([]int64{1, int64(NDFBins), 2})
	if err != nil {
		return nil, nil, err
	}
	defer dfTensor.Destroy()

	vadTensor, err := ort.NewEmptyTensor[float32]([]int64{1, 1})
	if err != nil {
		return nil, nil, err
	}
	defer vadTensor.Destroy()

	newHiddenTensor, err := ort.NewEmptyTensor[float32]([]int64{int64(GRULayers), 1, int64(GRUUnits)})
	if err != nil {
		return nil, nil, err
	}
	defer newHiddenTensor.Destroy()

	outputs := []ort.ArbitraryTensor{gainsTensor, dfTensor, vadTensor, newHiddenTensor}

	if err := s.session.Run(inputs, outputs); err != nil {
		return nil, nil, fmt.Errorf("inference failed: %w", err)
	}

	gains := make([]float32, NBands)
	copy(gains, gainsTensor.GetData())

	newHidden := make([]float32, GRULayers*1*GRUUnits)
	copy(newHidden, newHiddenTensor.GetData())

	return gains, newHidden, nil
}

// Destroy cleans up the ONNX session.
func (s *Suppressor) Destroy() {
	if s.session != nil {
		s.session.Destroy()
	}
}
