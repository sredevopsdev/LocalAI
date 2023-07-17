package tts

// This is a wrapper to statisfy the GRPC service interface
// It is meant to be used by the main executable that is the server for the specific backend type (falcon, gpt3, etc)
import (
	"os"

	"github.com/go-skynet/LocalAI/pkg/grpc/base"
	pb "github.com/go-skynet/LocalAI/pkg/grpc/proto"
	piper "github.com/mudler/go-piper"
)

type Piper struct {
	base.Base
	piper *PiperB
}

func (sd *Piper) Load(opts *pb.ModelOptions) error {
	var err error
	// Note: the Model here is a path to a directory containing the model files
	sd.piper, err = New(opts.LibrarySearchPath)
	return err
}

func (sd *Piper) TTS(opts *pb.TTSRequest) error {
	return sd.piper.TTS(opts.Text, opts.Model, opts.Dst)
}

type PiperB struct {
	assetDir string
}

func New(assetDir string) (*PiperB, error) {
	if _, err := os.Stat(assetDir); err != nil {
		return nil, err
	}
	return &PiperB{
		assetDir: assetDir,
	}, nil
}

func (s *PiperB) TTS(text, model, dst string) error {
	return piper.TextToWav(text, model, s.assetDir, "", dst)
}
