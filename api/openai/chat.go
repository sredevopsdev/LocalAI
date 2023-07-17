package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-skynet/LocalAI/api/backend"
	config "github.com/go-skynet/LocalAI/api/config"
	"github.com/go-skynet/LocalAI/api/options"
	"github.com/go-skynet/LocalAI/pkg/grammar"
	model "github.com/go-skynet/LocalAI/pkg/model"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
)

func ChatEndpoint(cm *config.ConfigLoader, o *options.Option) func(c *fiber.Ctx) error {
	emptyMessage := ""

	process := func(s string, req *OpenAIRequest, config *config.Config, loader *model.ModelLoader, responses chan OpenAIResponse) {
		initialMessage := OpenAIResponse{
			Model:   req.Model, // we have to return what the user sent here, due to OpenAI spec.
			Choices: []Choice{{Delta: &Message{Role: "assistant", Content: &emptyMessage}}},
			Object:  "chat.completion.chunk",
		}
		responses <- initialMessage

		ComputeChoices(s, req.N, config, o, loader, func(s string, c *[]Choice) {}, func(s string) bool {
			resp := OpenAIResponse{
				Model:   req.Model, // we have to return what the user sent here, due to OpenAI spec.
				Choices: []Choice{{Delta: &Message{Content: &s}, Index: 0}},
				Object:  "chat.completion.chunk",
			}

			responses <- resp
			return true
		})
		close(responses)
	}
	return func(c *fiber.Ctx) error {
		processFunctions := false
		funcs := grammar.Functions{}
		model, input, err := readInput(c, o.Loader, true)
		if err != nil {
			return fmt.Errorf("failed reading parameters from request:%w", err)
		}

		config, input, err := readConfig(model, input, cm, o.Loader, o.Debug, o.Threads, o.ContextSize, o.F16)
		if err != nil {
			return fmt.Errorf("failed reading parameters from request:%w", err)
		}
		log.Debug().Msgf("Configuration read: %+v", config)

		// Allow the user to set custom actions via config file
		// to be "embedded" in each model
		noActionName := "answer"
		noActionDescription := "use this action to answer without performing any action"

		if config.FunctionsConfig.NoActionFunctionName != "" {
			noActionName = config.FunctionsConfig.NoActionFunctionName
		}
		if config.FunctionsConfig.NoActionDescriptionName != "" {
			noActionDescription = config.FunctionsConfig.NoActionDescriptionName
		}

		// process functions if we have any defined or if we have a function call string
		if len(input.Functions) > 0 && config.ShouldUseFunctions() {
			log.Debug().Msgf("Response needs to process functions")

			processFunctions = true

			noActionGrammar := grammar.Function{
				Name:        noActionName,
				Description: noActionDescription,
				Parameters: map[string]interface{}{
					"properties": map[string]interface{}{
						"message": map[string]interface{}{
							"type":        "string",
							"description": "The message to reply the user with",
						}},
				},
			}

			// Append the no action function
			funcs = append(funcs, input.Functions...)
			if !config.FunctionsConfig.DisableNoAction {
				funcs = append(funcs, noActionGrammar)
			}

			// Force picking one of the functions by the request
			if config.FunctionToCall() != "" {
				funcs = funcs.Select(config.FunctionToCall())
			}

			// Update input grammar
			jsStruct := funcs.ToJSONStructure()
			config.Grammar = jsStruct.Grammar("")
		} else if input.JSONFunctionGrammarObject != nil {
			config.Grammar = input.JSONFunctionGrammarObject.Grammar("")
		}

		// functions are not supported in stream mode (yet?)
		toStream := input.Stream && !processFunctions

		log.Debug().Msgf("Parameters: %+v", config)

		var predInput string

		mess := []string{}
		for _, i := range input.Messages {
			var content string
			role := i.Role
			// if function call, we might want to customize the role so we can display better that the "assistant called a json action"
			// if an "assistant_function_call" role is defined, we use it, otherwise we use the role that is passed by in the request
			if i.FunctionCall != nil && i.Role == "assistant" {
				roleFn := "assistant_function_call"
				r := config.Roles[roleFn]
				if r != "" {
					role = roleFn
				}
			}
			r := config.Roles[role]
			contentExists := i.Content != nil && *i.Content != ""
			if r != "" {
				if contentExists {
					content = fmt.Sprint(r, " ", *i.Content)
				}
				if i.FunctionCall != nil {
					j, err := json.Marshal(i.FunctionCall)
					if err == nil {
						if contentExists {
							content += "\n" + fmt.Sprint(r, " ", string(j))
						} else {
							content = fmt.Sprint(r, " ", string(j))
						}
					}
				}
			} else {
				if contentExists {
					content = fmt.Sprint(*i.Content)
				}
				if i.FunctionCall != nil {
					j, err := json.Marshal(i.FunctionCall)
					if err == nil {
						if contentExists {
							content += "\n" + string(j)
						} else {
							content = string(j)
						}
					}
				}
			}

			mess = append(mess, content)
		}

		predInput = strings.Join(mess, "\n")
		log.Debug().Msgf("Prompt (before templating): %s", predInput)

		if toStream {
			log.Debug().Msgf("Stream request received")
			c.Context().SetContentType("text/event-stream")
			//c.Response().Header.SetContentType(fiber.MIMETextHTMLCharsetUTF8)
			//	c.Set("Content-Type", "text/event-stream")
			c.Set("Cache-Control", "no-cache")
			c.Set("Connection", "keep-alive")
			c.Set("Transfer-Encoding", "chunked")
		}

		templateFile := config.Model

		if config.TemplateConfig.Chat != "" && !processFunctions {
			templateFile = config.TemplateConfig.Chat
		}

		if config.TemplateConfig.Functions != "" && processFunctions {
			templateFile = config.TemplateConfig.Functions
		}

		// A model can have a "file.bin.tmpl" file associated with a prompt template prefix
		templatedInput, err := o.Loader.TemplatePrefix(templateFile, struct {
			Input     string
			Functions []grammar.Function
		}{
			Input:     predInput,
			Functions: funcs,
		})
		if err == nil {
			predInput = templatedInput
			log.Debug().Msgf("Template found, input modified to: %s", predInput)
		} else {
			log.Debug().Msgf("Template failed loading: %s", err.Error())
		}

		log.Debug().Msgf("Prompt (after templating): %s", predInput)
		if processFunctions {
			log.Debug().Msgf("Grammar: %+v", config.Grammar)
		}

		if toStream {
			responses := make(chan OpenAIResponse)

			go process(predInput, input, config, o.Loader, responses)

			c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {

				for ev := range responses {
					var buf bytes.Buffer
					enc := json.NewEncoder(&buf)
					enc.Encode(ev)

					log.Debug().Msgf("Sending chunk: %s", buf.String())
					fmt.Fprintf(w, "data: %v\n", buf.String())
					w.Flush()
				}

				resp := &OpenAIResponse{
					Model: input.Model, // we have to return what the user sent here, due to OpenAI spec.
					Choices: []Choice{
						{
							FinishReason: "stop",
							Index:        0,
							Delta:        &Message{Content: &emptyMessage},
						}},
					Object: "chat.completion.chunk",
				}
				respData, _ := json.Marshal(resp)

				w.WriteString(fmt.Sprintf("data: %s\n\n", respData))
				w.WriteString("data: [DONE]\n\n")
				w.Flush()
			}))
			return nil
		}

		result, err := ComputeChoices(predInput, input.N, config, o, o.Loader, func(s string, c *[]Choice) {
			if processFunctions {
				// As we have to change the result before processing, we can't stream the answer (yet?)
				ss := map[string]interface{}{}
				json.Unmarshal([]byte(s), &ss)
				log.Debug().Msgf("Function return: %s %+v", s, ss)

				// The grammar defines the function name as "function", while OpenAI returns "name"
				func_name := ss["function"]
				// Similarly, while here arguments is a map[string]interface{}, OpenAI actually want a stringified object
				args := ss["arguments"] // arguments needs to be a string, but we return an object from the grammar result (TODO: fix)
				d, _ := json.Marshal(args)

				ss["arguments"] = string(d)
				ss["name"] = func_name

				// if do nothing, reply with a message
				if func_name == noActionName {
					log.Debug().Msgf("nothing to do, computing a reply")

					// If there is a message that the LLM already sends as part of the JSON reply, use it
					arguments := map[string]interface{}{}
					json.Unmarshal([]byte(d), &arguments)
					m, exists := arguments["message"]
					if exists {
						switch message := m.(type) {
						case string:
							if message != "" {
								log.Debug().Msgf("Reply received from LLM: %s", message)
								message = backend.Finetune(*config, predInput, message)
								log.Debug().Msgf("Reply received from LLM(finetuned): %s", message)

								*c = append(*c, Choice{Message: &Message{Role: "assistant", Content: &message}})
								return
							}
						}
					}

					log.Debug().Msgf("No action received from LLM, without a message, computing a reply")
					// Otherwise ask the LLM to understand the JSON output and the context, and return a message
					// Note: This costs (in term of CPU) another computation
					config.Grammar = ""
					predFunc, err := backend.ModelInference(predInput, o.Loader, *config, o, nil)
					if err != nil {
						log.Error().Msgf("inference error: %s", err.Error())
						return
					}

					prediction, err := predFunc()
					if err != nil {
						log.Error().Msgf("inference error: %s", err.Error())
						return
					}

					prediction = backend.Finetune(*config, predInput, prediction)
					*c = append(*c, Choice{Message: &Message{Role: "assistant", Content: &prediction}})
				} else {
					// otherwise reply with the function call
					*c = append(*c, Choice{
						FinishReason: "function_call",
						Message:      &Message{Role: "assistant", FunctionCall: ss},
					})
				}

				return
			}
			*c = append(*c, Choice{Message: &Message{Role: "assistant", Content: &s}})
		}, nil)
		if err != nil {
			return err
		}

		resp := &OpenAIResponse{
			Model:   input.Model, // we have to return what the user sent here, due to OpenAI spec.
			Choices: result,
			Object:  "chat.completion",
		}
		respData, _ := json.Marshal(resp)
		log.Debug().Msgf("Response: %s", respData)

		// Return the prediction in the response body
		return c.JSON(resp)
	}
}
