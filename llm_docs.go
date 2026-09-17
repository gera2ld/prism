package main

import (
	"github.com/danielgtaylor/huma/v2"
)

// clientKeySecurity authenticates the LLM endpoints with a client API key
// (sk-…) as a Bearer token, as opposed to the superuser scheme used by the
// operational endpoints.
var clientKeySecurity = []map[string][]string{{"clientKey": {}}}

func strProp(desc string) *huma.Schema {
	return &huma.Schema{Type: "string", Description: desc}
}

func intProp(desc string) *huma.Schema {
	return &huma.Schema{Type: "integer", Description: desc}
}

func boolProp(desc string) *huma.Schema {
	return &huma.Schema{Type: "boolean", Description: desc}
}

func objProp(desc string, required []string, props map[string]*huma.Schema) *huma.Schema {
	return &huma.Schema{
		Type:        "object",
		Description: desc,
		Required:    required,
		Properties:  props,
	}
}

var chatMessageSchema = objProp("A conversation message.", []string{"role", "content"}, map[string]*huma.Schema{
	"role":    strProp("Message role, e.g. system, user, assistant, tool."),
	"content": strProp("Message content. Upstream shapes (e.g. content-part arrays) pass through untouched."),
})

var chatRequestSchema = &huma.Schema{
	Type:        "object",
	Description: "Only model and stream are read by the gateway; every other OpenAI chat field passes through untouched. The model is rewritten to the upstream model and stream_options.include_usage is injected on streams.",
	Required:    []string{"model", "messages"},
	Properties: map[string]*huma.Schema{
		"model":       strProp("Prism alias as configured in the routing table, e.g. fast."),
		"messages":    {Type: "array", Description: "Conversation history.", Items: chatMessageSchema},
		"stream":      boolProp("Stream server-sent events instead of a single JSON body."),
		"temperature": {Type: "number", Description: "Sampling temperature, passed through."},
		"max_tokens":  intProp("Maximum completion tokens, passed through."),
		"top_p":       {Type: "number", Description: "Nucleus sampling cutoff, passed through."},
		"stream_options": objProp("Streaming options, passed through.", nil, map[string]*huma.Schema{
			"include_usage": boolProp("Request a final usage chunk. The gateway forces this on."),
		}),
	},
	AdditionalProperties: true,
}

var usageSchema = objProp("Token usage as reported by the provider; absent fields mean unknown.", nil, map[string]*huma.Schema{
	"prompt_tokens":     intProp("Prompt tokens."),
	"completion_tokens": intProp("Completion tokens."),
	"total_tokens":      intProp("Total tokens."),
	"prompt_tokens_details": objProp("Prompt token details.", nil, map[string]*huma.Schema{
		"cached_tokens": intProp("Prompt-cache hits, a subset of prompt_tokens."),
	}),
})

var choiceSchema = objProp("A single completion choice.", nil, map[string]*huma.Schema{
	"index": intProp("Choice index."),
	"message": objProp("The assistant message (non-streaming).", nil, map[string]*huma.Schema{
		"role":    strProp("Message role."),
		"content": strProp("Message content."),
	}),
	"finish_reason": strProp("Why generation stopped, e.g. stop, length, tool_calls. Null in stream chunks until the final one."),
})

var chatResponseSchema = objProp("Chat completion, relayed byte-identical from the upstream.", nil, map[string]*huma.Schema{
	"id":      strProp("Completion ID."),
	"object":  strProp("Always chat.completion."),
	"created": intProp("Unix timestamp."),
	"model":   strProp("Upstream model name."),
	"choices": {Type: "array", Description: "Completion choices.", Items: choiceSchema},
	"usage":   usageSchema,
})

var chatChunkSchema = objProp("One server-sent event payload (after the data: prefix). The stream ends with a data: [DONE] line. Usage, when the provider honors stream_options, arrives in the final chunk.", nil, map[string]*huma.Schema{
	"id":      strProp("Completion ID."),
	"object":  strProp("Always chat.completion.chunk."),
	"created": intProp("Unix timestamp."),
	"model":   strProp("Upstream model name."),
	"choices": {Type: "array", Description: "Delta choices.", Items: objProp("A delta choice.", nil, map[string]*huma.Schema{
		"index": intProp("Choice index."),
		"delta": objProp("Incremental content.", nil, map[string]*huma.Schema{
			"role":    strProp("Role, usually only in the first chunk."),
			"content": strProp("Content fragment."),
		}),
		"finish_reason": strProp("Set on the final chunk only."),
	})},
	"usage": usageSchema,
})

var modelsResponseSchema = objProp("Usable aliases for the presented key.", nil, map[string]*huma.Schema{
	"object": strProp("Always list."),
	"data": {Type: "array", Description: "Aliases the key may use.", Items: objProp("A usable alias.", nil, map[string]*huma.Schema{
		"object":   strProp("Always model."),
		"id":       strProp("Alias."),
		"owned_by": strProp("Provider that would serve the alias."),
	})},
})

var gatewayErrorSchema = objProp("Gateway error envelope.", nil, map[string]*huma.Schema{
	"error": objProp("Error detail.", nil, map[string]*huma.Schema{
		"message": strProp("Human-readable message."),
		"type":    strProp("Always invalid_request_error."),
		"code":    strProp("Machine-readable code, e.g. invalid_api_key, unknown_model, forbidden, upstream_error."),
	}),
})

func gatewayErrorResponses() map[string]*huma.Response {
	err := func(desc string) *huma.Response {
		return &huma.Response{
			Description: desc,
			Content: map[string]*huma.MediaType{
				"application/json": {Schema: gatewayErrorSchema},
			},
		}
	}
	return map[string]*huma.Response{
		"400": err("Malformed JSON body or body too large."),
		"401": err("Missing or unknown client API key."),
		"403": err("Key is not allowed to use the model."),
		"404": err("No route for the model alias."),
		"502": err("Upstream request failed."),
	}
}

// registerLLMDocs documents the pass-through LLM endpoints in the shared
// spec. It only writes documentation: traffic keeps flowing through the
// proxy untouched, so there is no validation or behavior risk.
func registerLLMDocs(api huma.API) {
	openAPI := api.OpenAPI()
	if openAPI.Paths == nil {
		openAPI.Paths = map[string]*huma.PathItem{}
	}
	openAPI.Paths["/v1/chat/completions"] = &huma.PathItem{
		Post: &huma.Operation{
			OperationID: "create-chat-completion",
			Summary:     "Create a chat completion",
			Description: "OpenAI-compatible chat completions over the configured providers. The model field names a Prism alias; responses are relayed byte-identical from the upstream.",
			Tags:        []string{"llm"},
			Security:    clientKeySecurity,
			RequestBody: &huma.RequestBody{
				Required: true,
				Content: map[string]*huma.MediaType{
					"application/json": {Schema: chatRequestSchema},
				},
			},
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["200"] = &huma.Response{
					Description: "Completion JSON, or server-sent events when stream is true.",
					Content: map[string]*huma.MediaType{
						"application/json":  {Schema: chatResponseSchema},
						"text/event-stream": {Schema: chatChunkSchema},
					},
				}
				return responses
			}(),
		},
	}
	openAPI.Paths["/v1/models"] = &huma.PathItem{
		Get: &huma.Operation{
			OperationID: "list-models",
			Summary:     "List usable aliases",
			Description: "Aliases the presented key may use, in OpenAI's list shape.",
			Tags:        []string{"llm"},
			Security:    clientKeySecurity,
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["200"] = &huma.Response{
					Description: "Usable aliases.",
					Content: map[string]*huma.MediaType{
						"application/json": {Schema: modelsResponseSchema},
					},
				}
				return responses
			}(),
		},
	}
}
