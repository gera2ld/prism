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
		"object":                   strProp("Always model."),
		"id":                       strProp("Alias."),
		"owned_by":                 strProp("Provider that would serve the alias."),
		"supported_endpoint_types": {Type: "array", Description: "Endpoint types this gateway exposes for the alias, in new-api's vocabulary: openai, image-generation, or both.", Items: &huma.Schema{Type: "string"}},
	})},
})

var imageRequestSchema = &huma.Schema{
	Type:        "object",
	Description: "Only model and prompt are read by the gateway; every other field passes through untouched. The model names a Prism alias backed by an image route and is rewritten to the upstream model before forwarding to {base_url}/images.",
	Required:    []string{"model", "prompt"},
	Properties: map[string]*huma.Schema{
		"model":            strProp("Prism alias as configured in the routing table, e.g. fast-image."),
		"prompt":           strProp("Text description of the desired image."),
		"n":                intProp("Upper bound on images to generate. Providers may return fewer."),
		"aspect_ratio":     strProp("Normalized aspect ratio, e.g. 16:9. Providers clamp to their supported subset."),
		"resolution":       strProp("Resolution tier, e.g. 1K, 2K."),
		"size":             strProp("Convenience shorthand, a tier or explicit pixels."),
		"quality":          strProp("auto, low, medium, or high."),
		"output_format":    strProp("png, jpeg, webp, or svg. When omitted, the provider default applies."),
		"background":       strProp("auto, transparent, or opaque."),
		"seed":             intProp("Seed for deterministic generation, where supported."),
		"input_references": {Type: "array", Description: "Reference images for image-to-image generation, as base64 data URLs or HTTP(S) URLs.", Items: &huma.Schema{Type: "object", AdditionalProperties: true}},
	},
	AdditionalProperties: true,
}

var imageResponseSchema = objProp("Image generation result, relayed byte-identical from the upstream.", nil, map[string]*huma.Schema{"created": intProp("Unix timestamp (seconds) when the image was generated."),
	"data": {Type: "array", Description: "Generated images.", Items: objProp("One image.", nil, map[string]*huma.Schema{
		"b64_json":       strProp("Base64-encoded image bytes."),
		"media_type":     strProp("Present whenever the format is identifiable, e.g. image/png."),
		"revised_prompt": strProp("Provider-rewritten prompt, when returned."),
	})},
	"usage": objProp("Token usage as reported by the provider; absent fields mean unknown. The provider cost field, when present, is passed through and logged.", nil, map[string]*huma.Schema{
		"prompt_tokens":     intProp("Prompt tokens."),
		"completion_tokens": intProp("Completion tokens."),
		"total_tokens":      intProp("Total tokens."),
	}),
})

var openAIImageRequestSchema = &huma.Schema{
	Type:        "object",
	Description: "Only model and prompt are read by the gateway; every other OpenAI field passes through untouched. The model names a Prism alias backed by an image route and is rewritten to the upstream model before forwarding to {base_url}/images/generations.",
	Required:    []string{"model", "prompt"},
	Properties: map[string]*huma.Schema{
		"model":           strProp("Prism alias as configured in the routing table, e.g. fast-image."),
		"prompt":          strProp("Text description of the desired image."),
		"n":               intProp("Number of images to generate."),
		"size":            strProp("Image size, e.g. 1024x1024. Supported values depend on the upstream model."),
		"quality":         strProp("Image quality, e.g. standard, hd, auto, low, medium, high."),
		"style":           strProp("Image style, e.g. vivid or natural."),
		"response_format": strProp("url or b64_json. When omitted, the provider default applies."),
		"user":            strProp("End-user identifier for abuse monitoring, passed through."),
	},
	AdditionalProperties: true,
}

var openAIImageResponseSchema = objProp("Image generation result, relayed byte-identical from the upstream.", nil, map[string]*huma.Schema{"created": intProp("Unix timestamp when the image was generated."),
	"data": {Type: "array", Description: "Generated images.", Items: objProp("One image.", nil, map[string]*huma.Schema{
		"url":            strProp("URL of the generated image, when response_format is url."),
		"b64_json":       strProp("Base64-encoded image bytes, when response_format is b64_json."),
		"revised_prompt": strProp("Provider-rewritten prompt, when returned."),
	})},
	"usage": objProp("Token usage as reported by the provider; absent when the provider reports none.", nil, map[string]*huma.Schema{
		"prompt_tokens":     intProp("Prompt tokens."),
		"completion_tokens": intProp("Completion tokens."),
		"total_tokens":      intProp("Total tokens."),
	}),
})

var editsRequestSchema = objProp("Multipart image edit. Only model, prompt and image are read by the gateway; every other field and file passes through untouched. The model names a Prism alias backed by an image route and is rewritten to the upstream model before forwarding to {base_url}/images/edits.", []string{"model", "prompt", "image"}, map[string]*huma.Schema{
	"model":  strProp("Prism alias as configured in the routing table, e.g. fast-image."),
	"prompt": strProp("Text description of the desired edit."),
	"image":  {Type: "string", Format: "binary", Description: "The image to edit, as a PNG file."},
	"mask":   {Type: "string", Format: "binary", Description: "Optional mask: transparent areas are edited, opaque areas kept."},
	"n":      intProp("Number of images to generate."),
	"size":   strProp("Image size, e.g. 1024x1024. Supported values depend on the upstream model."),
})

var gatewayErrorSchema = objProp("Gateway error envelope.", nil, map[string]*huma.Schema{
	"error": objProp("Error detail.", nil, map[string]*huma.Schema{
		"message": strProp("Human-readable message."),
		"type":    strProp("Always invalid_request_error."),
		"code":    strProp("Machine-readable code, e.g. invalid_api_key, unknown_model, unknown_tool, forbidden, upstream_error."),
	}),
})

var toolsResponseSchema = objProp("Tools this key may call, in OpenAI's tool shape.", nil, map[string]*huma.Schema{
	"object": strProp("Always list."),
	"data": {Type: "array", Description: "Callable tools. MCP tools appear only while approved under a hash that still matches the server's current definition.", Items: objProp("One callable tool.", nil, map[string]*huma.Schema{
		"id":   strProp("Same as function.name; makes the listing addressable."),
		"type": strProp("Always function."),
		"function": objProp("The tool as the model sees it.", []string{"name"}, map[string]*huma.Schema{
			"name":        strProp("Name to call, e.g. mcp__github__create_issue for a namespaced MCP tool."),
			"description": strProp("What the tool does, from the tool's own definition."),
			"parameters":  {Type: "object", Description: "JSON Schema for the arguments. Absent when the tool publishes no schema.", AdditionalProperties: true},
		}),
	})},
})

var invokeRequestSchema = objProp("Arguments for one tool call.", nil, map[string]*huma.Schema{
	"arguments": {Type: "object", Description: "Arguments matching the tool's schema. Omit or pass null for a tool that takes none.", AdditionalProperties: true},
})

var invokeResponseSchema = objProp("The outcome of one tool call.", nil, map[string]*huma.Schema{
	"object":   strProp("Always tool.result."),
	"tool":     strProp("Name that was invoked."),
	"is_error": boolProp("The tool ran and failed; result carries the reason. Still a 200, so an agent can feed it back as a tool message."),
	"result":   {Description: "The tool's output: whatever JSON value its definition produced, or the text an MCP tool returned. Null when the tool returned nothing."},
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
	openAPI.Paths["/v1/images"] = &huma.PathItem{
		Post: &huma.Operation{
			OperationID: "create-image",
			Summary:     "Generate images (OpenRouter shape)",
			Description: "Image generation over the configured providers, in OpenRouter's shape. The model field names a Prism alias backed by an image route; responses are relayed byte-identical from the upstream. Streaming is not supported. Prefer POST /v1/images/generations for the OpenAI shape.",
			Tags:        []string{"llm"},
			Security:    clientKeySecurity,
			RequestBody: &huma.RequestBody{
				Required: true,
				Content: map[string]*huma.MediaType{
					"application/json": {Schema: imageRequestSchema},
				},
			},
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["200"] = &huma.Response{
					Description: "Generated images as base64 bytes with usage.",
					Content: map[string]*huma.MediaType{
						"application/json": {Schema: imageResponseSchema},
					},
				}
				return responses
			}(),
		},
	}
	openAPI.Paths["/v1/images/generations"] = &huma.PathItem{
		Post: &huma.Operation{
			OperationID: "create-image-generation",
			Summary:     "Generate images (OpenAI shape)",
			Description: "Image generation over the configured providers, in OpenAI's images/generations shape. The model field names a Prism alias backed by an image route; responses are relayed byte-identical from the upstream.",
			Tags:        []string{"llm"},
			Security:    clientKeySecurity,
			RequestBody: &huma.RequestBody{
				Required: true,
				Content: map[string]*huma.MediaType{
					"application/json": {Schema: openAIImageRequestSchema},
				},
			},
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["200"] = &huma.Response{
					Description: "Generated images as URLs or base64 bytes.",
					Content: map[string]*huma.MediaType{
						"application/json": {Schema: openAIImageResponseSchema},
					},
				}
				return responses
			}(),
		},
	}
	openAPI.Paths["/v1/images/edits"] = &huma.PathItem{
		Post: &huma.Operation{
			OperationID: "create-image-edit",
			Summary:     "Edit images (OpenAI shape)",
			Description: "Image edits over the configured providers, in OpenAI's multipart images/edits shape. The model field names a Prism alias backed by an image route; uploaded images are forwarded with the model rewritten, and responses are relayed byte-identical from the upstream. Request transformers do not apply to multipart bodies.",
			Tags:        []string{"llm"},
			Security:    clientKeySecurity,
			RequestBody: &huma.RequestBody{
				Required: true,
				Content: map[string]*huma.MediaType{
					"multipart/form-data": {Schema: editsRequestSchema},
				},
			},
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["200"] = &huma.Response{
					Description: "Edited images as URLs or base64 bytes.",
					Content: map[string]*huma.MediaType{
						"application/json": {Schema: openAIImageResponseSchema},
					},
				}
				return responses
			}(),
		},
	}
	registerToolDocs(openAPI)
}

// registerToolDocs documents the tools surface an agent drives. Prism does not
// run the agent's loop: it lists the tools it may call and executes one on
// request, which keeps the gateway a pass-through for chat and leaves the loop
// to the client.
func registerToolDocs(openAPI *huma.OpenAPI) {
	openAPI.Paths["/v1/tools"] = &huma.PathItem{
		Get: &huma.Operation{
			OperationID: "list-tools",
			Summary:     "List callable tools",
			Description: "The tools this key may call, in OpenAI's tool shape so the array can be handed straight to a chat request's tools field.\n\n" +
				"Built from your own conduit definitions, where each document supplies the name, description and argument schema. A tool that is disabled, or whose definition no longer compiles, is simply absent. The same tools are also served over MCP at /mcp.",
			Tags:     []string{"tools"},
			Security: clientKeySecurity,
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["200"] = &huma.Response{
					Description: "Callable tools.",
					Content: map[string]*huma.MediaType{
						"application/json": {Schema: toolsResponseSchema},
					},
				}
				return responses
			}(),
		},
	}
	openAPI.Paths["/v1/tools/{name}/invoke"] = &huma.PathItem{
		Post: &huma.Operation{
			OperationID: "invoke-tool",
			Summary:     "Invoke a tool",
			Description: "Runs one tool and returns its output.\n\n" +
				"A tool that ran and failed is still a 200 with is_error true and the reason in result, because an agent needs to read that and correct itself. Only a gateway-level fault, or a name that is not callable, is a non-200.",
			Tags:     []string{"tools"},
			Security: clientKeySecurity,
			RequestBody: &huma.RequestBody{
				Required: true,
				Content: map[string]*huma.MediaType{
					"application/json": {Schema: invokeRequestSchema},
				},
			},
			Responses: func() map[string]*huma.Response {
				responses := gatewayErrorResponses()
				responses["404"] = &huma.Response{
					Description: "No callable tool by that name.",
					Content:     map[string]*huma.MediaType{"application/json": {Schema: gatewayErrorSchema}},
				}
				responses["200"] = &huma.Response{
					Description: "The tool's output.",
					Content: map[string]*huma.MediaType{
						"application/json": {Schema: invokeResponseSchema},
					},
				}
				return responses
			}(),
		},
	}
}
