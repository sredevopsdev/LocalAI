package model

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/go-skynet/LocalAI/pkg/grpc"
	process "github.com/mudler/go-processmanager"
	"github.com/rs/zerolog/log"
)

type ModelLoader struct {
	ModelPath string
	mu        sync.Mutex
	// TODO: this needs generics
	models           map[string]*grpc.Client
	grpcProcesses    map[string]*process.Process
	promptsTemplates map[string]*template.Template
}

func NewModelLoader(modelPath string) *ModelLoader {
	return &ModelLoader{
		ModelPath:        modelPath,
		models:           make(map[string]*grpc.Client),
		promptsTemplates: make(map[string]*template.Template),
		grpcProcesses:    make(map[string]*process.Process),
	}
}

func (ml *ModelLoader) ExistsInModelPath(s string) bool {
	_, err := os.Stat(filepath.Join(ml.ModelPath, s))
	return err == nil
}

func (ml *ModelLoader) ListModels() ([]string, error) {
	files, err := ioutil.ReadDir(ml.ModelPath)
	if err != nil {
		return []string{}, err
	}

	models := []string{}
	for _, file := range files {
		// Skip templates, YAML and .keep files
		if strings.HasSuffix(file.Name(), ".tmpl") || strings.HasSuffix(file.Name(), ".keep") || strings.HasSuffix(file.Name(), ".yaml") || strings.HasSuffix(file.Name(), ".yml") {
			continue
		}

		models = append(models, file.Name())
	}

	return models, nil
}

func (ml *ModelLoader) TemplatePrefix(modelName string, in interface{}) (string, error) {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	m, ok := ml.promptsTemplates[modelName]
	if !ok {
		modelFile := filepath.Join(ml.ModelPath, modelName)
		if err := ml.loadTemplateIfExists(modelName, modelFile); err != nil {
			return "", err
		}

		t, exists := ml.promptsTemplates[modelName]
		if exists {
			m = t
		}
	}
	if m == nil {
		return "", fmt.Errorf("failed loading any template")
	}

	var buf bytes.Buffer

	if err := m.Execute(&buf, in); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (ml *ModelLoader) loadTemplateIfExists(modelName, modelFile string) error {
	// Check if the template was already loaded
	if _, ok := ml.promptsTemplates[modelName]; ok {
		return nil
	}

	// Check if the model path exists
	// skip any error here - we run anyway if a template does not exist
	modelTemplateFile := fmt.Sprintf("%s.tmpl", modelName)

	if !ml.ExistsInModelPath(modelTemplateFile) {
		return nil
	}

	dat, err := os.ReadFile(filepath.Join(ml.ModelPath, modelTemplateFile))
	if err != nil {
		return err
	}

	// Parse the template
	tmpl, err := template.New("prompt").Parse(string(dat))
	if err != nil {
		return err
	}
	ml.promptsTemplates[modelName] = tmpl

	return nil
}

func (ml *ModelLoader) LoadModel(modelName string, loader func(string) (*grpc.Client, error)) (*grpc.Client, error) {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	// Check if we already have a loaded model
	if model := ml.checkIsLoaded(modelName); model != nil {
		log.Debug().Msgf("Model already loaded in memory: %s", modelName)
		return model, nil
	}

	// Load the model and keep it in memory for later use
	modelFile := filepath.Join(ml.ModelPath, modelName)
	log.Debug().Msgf("Loading model in memory from file: %s", modelFile)

	model, err := loader(modelFile)
	if err != nil {
		return nil, err
	}

	// If there is a prompt template, load it
	if err := ml.loadTemplateIfExists(modelName, modelFile); err != nil {
		return nil, err
	}

	ml.models[modelName] = model
	return model, nil
}

func (ml *ModelLoader) checkIsLoaded(s string) *grpc.Client {
	if m, ok := ml.models[s]; ok {
		log.Debug().Msgf("Model already loaded in memory: %s", s)

		if !m.HealthCheck(context.Background()) {
			log.Debug().Msgf("GRPC Model not responding", s)
			if !ml.grpcProcesses[s].IsAlive() {
				log.Debug().Msgf("GRPC Process is not responding", s)
				// stop and delete the process, this forces to re-load the model and re-create again the service
				ml.grpcProcesses[s].Stop()
				delete(ml.grpcProcesses, s)
				delete(ml.models, s)
				return nil
			}
		}

		return m
	}

	return nil
}
