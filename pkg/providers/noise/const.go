package noise

// Model configuration matching the v2 training setup
const (
	SampleRate  = 16000
	NFFT        = 512
	HopLength   = 128
	NBands      = 34
	NDFBins     = 96
	NFeatures   = 78
	GRUUnits    = 256
	GRULayers   = 3
)
