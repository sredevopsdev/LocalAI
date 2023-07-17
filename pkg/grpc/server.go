package grpc

import (
	"context"
	"fmt"
	"log"
	"net"

	pb "github.com/go-skynet/LocalAI/pkg/grpc/proto"
	"google.golang.org/grpc"
)

// A GRPC Server that allows to run LLM inference.
// It is used by the LLMServices to expose the LLM functionalities that are called by the client.
// The GRPC Service is general, trying to encompass all the possible LLM options models.
// It depends on the real implementer then what can be done or not.
//
// The server is implemented as a GRPC service, with the following methods:
// - Predict: to run the inference with options
// - PredictStream: to run the inference with options and stream the results

// server is used to implement helloworld.GreeterServer.
type server struct {
	pb.UnimplementedBackendServer
	llm LLM
}

func (s *server) Health(ctx context.Context, in *pb.HealthMessage) (*pb.Reply, error) {
	return &pb.Reply{Message: "OK"}, nil
}

func (s *server) Embedding(ctx context.Context, in *pb.PredictOptions) (*pb.EmbeddingResult, error) {
	embeds, err := s.llm.Embeddings(in)
	if err != nil {
		return nil, err
	}

	return &pb.EmbeddingResult{Embeddings: embeds}, nil
}

func (s *server) LoadModel(ctx context.Context, in *pb.ModelOptions) (*pb.Result, error) {
	err := s.llm.Load(in)
	if err != nil {
		return &pb.Result{Message: fmt.Sprintf("Error loading model: %s", err.Error()), Success: false}, err
	}
	return &pb.Result{Message: "Loading succeeded", Success: true}, nil
}

func (s *server) Predict(ctx context.Context, in *pb.PredictOptions) (*pb.Reply, error) {
	result, err := s.llm.Predict(in)
	return &pb.Reply{Message: result}, err
}

func (s *server) GenerateImage(ctx context.Context, in *pb.GenerateImageRequest) (*pb.Result, error) {
	err := s.llm.GenerateImage(in)
	if err != nil {
		return &pb.Result{Message: fmt.Sprintf("Error generating image: %s", err.Error()), Success: false}, err
	}
	return &pb.Result{Message: "Image generated", Success: true}, nil
}

func (s *server) TTS(ctx context.Context, in *pb.TTSRequest) (*pb.Result, error) {
	err := s.llm.TTS(in)
	if err != nil {
		return &pb.Result{Message: fmt.Sprintf("Error generating audio: %s", err.Error()), Success: false}, err
	}
	return &pb.Result{Message: "Audio generated", Success: true}, nil
}

func (s *server) AudioTranscription(ctx context.Context, in *pb.TranscriptRequest) (*pb.TranscriptResult, error) {
	result, err := s.llm.AudioTranscription(in)
	if err != nil {
		return nil, err
	}
	tresult := &pb.TranscriptResult{}
	for _, s := range result.Segments {
		tks := []int32{}
		for _, t := range s.Tokens {
			tks = append(tks, int32(t))
		}
		tresult.Segments = append(tresult.Segments,
			&pb.TranscriptSegment{
				Text:   s.Text,
				Id:     int32(s.Id),
				Start:  int64(s.Start),
				End:    int64(s.End),
				Tokens: tks,
			})
	}

	tresult.Text = result.Text
	return tresult, nil
}

func (s *server) PredictStream(in *pb.PredictOptions, stream pb.Backend_PredictStreamServer) error {

	resultChan := make(chan string)

	done := make(chan bool)
	go func() {
		for result := range resultChan {
			stream.Send(&pb.Reply{Message: result})
		}
		done <- true
	}()

	s.llm.PredictStream(in, resultChan)
	<-done

	return nil
}

func StartServer(address string, model LLM) error {
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	s := grpc.NewServer()
	pb.RegisterBackendServer(s, &server{llm: model})
	log.Printf("gRPC Server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		return err
	}

	return nil
}
