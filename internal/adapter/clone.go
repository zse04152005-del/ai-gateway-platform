package adapter

import "encoding/json"

// Clone returns a deep copy suitable for an attempt-local adapter invocation.
func (request NormalizedRequest) Clone() NormalizedRequest {
	cloned := request
	cloned.Messages = cloneMessages(request.Messages)
	cloned.Temperature = cloneFloat64(request.Temperature)
	cloned.TopP = cloneFloat64(request.TopP)
	cloned.MaxOutputTokens = cloneInt64(request.MaxOutputTokens)
	cloned.Stop = append([]string(nil), request.Stop...)
	cloned.Tools = cloneTools(request.Tools)
	cloned.ToolChoice = cloneToolChoice(request.ToolChoice)
	cloned.ResponseFormat = cloneResponseFormat(request.ResponseFormat)
	cloned.PolicyLabels = append([]string(nil), request.PolicyLabels...)
	cloned.ProviderOptions = cloneJSON(request.ProviderOptions)
	return cloned
}

// Clone returns a deep copy of a complete normalized response.
func (response NormalizedResponse) Clone() NormalizedResponse {
	cloned := response
	cloned.Choices = make([]NormalizedChoice, len(response.Choices))
	for index := range response.Choices {
		cloned.Choices[index] = response.Choices[index]
		cloned.Choices[index].Message = cloneMessage(response.Choices[index].Message)
	}
	if response.Usage != nil {
		usage := response.Usage.Clone()
		cloned.Usage = &usage
	}
	return cloned
}

// Clone returns a deep copy of one normalized stream event.
func (chunk NormalizedChunk) Clone() NormalizedChunk {
	cloned := chunk
	if chunk.ToolDelta != nil {
		toolDelta := *chunk.ToolDelta
		cloned.ToolDelta = &toolDelta
	}
	if chunk.Usage != nil {
		usage := chunk.Usage.Clone()
		cloned.Usage = &usage
	}
	cloned.ProviderExtension = cloneJSON(chunk.ProviderExtension)
	return cloned
}

// Clone returns a deep copy of usage metadata. Raw evidence remains immutable
// and can only be accessed through a defensive copy.
func (usage NormalizedUsage) Clone() NormalizedUsage {
	cloned := usage
	if usage.Estimate != nil {
		estimate := *usage.Estimate
		cloned.Estimate = &estimate
	}
	cloned.UnmappedFields = append([]string(nil), usage.UnmappedFields...)
	return cloned
}

// Clone returns a deep copy of safe normalized error facts.
func (normalizedError NormalizedError) Clone() NormalizedError {
	cloned := normalizedError
	if normalizedError.RetryAfter != nil {
		retryAfter := *normalizedError.RetryAfter
		cloned.RetryAfter = &retryAfter
	}
	return cloned
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	for index := range messages {
		cloned[index] = cloneMessage(messages[index])
	}
	return cloned
}

func cloneMessage(message Message) Message {
	cloned := message
	cloned.Parts = append([]ContentPart(nil), message.Parts...)
	cloned.ToolCalls = make([]ToolCall, len(message.ToolCalls))
	for index := range message.ToolCalls {
		cloned.ToolCalls[index] = message.ToolCalls[index]
		cloned.ToolCalls[index].Arguments = cloneJSON(message.ToolCalls[index].Arguments)
	}
	return cloned
}

func cloneTools(tools []ToolDefinition) []ToolDefinition {
	cloned := make([]ToolDefinition, len(tools))
	for index := range tools {
		cloned[index] = tools[index]
		cloned[index].InputSchema = cloneJSON(tools[index].InputSchema)
	}
	return cloned
}

func cloneToolChoice(choice *ToolChoice) *ToolChoice {
	if choice == nil {
		return nil
	}
	cloned := *choice
	return &cloned
}

func cloneResponseFormat(format *ResponseFormat) *ResponseFormat {
	if format == nil {
		return nil
	}
	cloned := *format
	cloned.Schema = cloneJSON(format.Schema)
	if format.Strict != nil {
		strict := *format.Strict
		cloned.Strict = &strict
	}
	return &cloned
}

func cloneJSON(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
