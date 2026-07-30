package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

const (
	maximumChatRequestBytes = 1 << 20
	maximumChatMessages     = 1024
	maximumChatTools        = 128
	maximumChatStops        = 16
	maximumTextBytes        = 1 << 20
	maximumSchemaBytes      = 64 << 10
)

var chatIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var chatToolNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,127}$`)
var safeParameterSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// parsedChatRequest is the validated client transport representation. It is
// deliberately separate from adapter.NormalizedRequest so transport parsing,
// normalization, policy enrichment, and provider selection remain auditable
// stages rather than one implicit conversion.
type parsedChatRequest struct {
	Model               string
	Messages            []parsedChatMessage
	Stream              bool
	Temperature         *float64
	TopP                *float64
	MaxCompletionTokens *int64
	Stop                []string
	Tools               []parsedChatTool
	ToolChoice          *parsedToolChoice
	ResponseFormat      *parsedResponseFormat
	User                string
}

type parsedChatMessage struct {
	Role       string
	Content    []parsedContentPart
	Name       string
	ToolCallID string
	ToolCalls  []parsedToolCall
}

type parsedContentPart struct {
	Kind        string
	Text        string
	ImageURL    string
	ImageDetail string
}

type parsedToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type parsedChatTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type parsedToolChoice struct {
	Mode string
	Name string
}

type parsedResponseFormat struct {
	Type        string
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

type requestProblem struct {
	status  int
	code    string
	message string
	param   string
}

func (problem *requestProblem) publicError() *apierror.Error {
	return apierror.MustNew(apierror.Definition{
		Status: problem.status, Code: problem.code, Message: problem.message,
		Type: "invalid_request_error", Param: problem.param,
	}, nil)
}

func newChatCompletionsHandler() http.Handler {
	methodError := newRequestProblem(
		http.StatusMethodNotAllowed,
		"METHOD_NOT_ALLOWED",
		"The HTTP method is not allowed for this resource",
		"",
	)
	notImplemented := apierror.MustNew(apierror.Definition{
		Status: http.StatusNotImplemented, Code: "CHAT_EXECUTION_NOT_IMPLEMENTED",
		Message: "Chat execution is not implemented yet", Type: "gateway_error",
	}, nil)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := correlation.RequestID(request.Context())
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			apierror.WriteHTTP(writer, methodError.publicError(), requestID, "gateway_error")
			return
		}
		if _, problem := parseChatCompletionRequest(writer, request); problem != nil {
			apierror.WriteHTTP(writer, problem.publicError(), requestID, "gateway_error")
			return
		}
		apierror.WriteHTTP(writer, notImplemented, requestID, "gateway_error")
	})
}

func parseChatCompletionRequest(writer http.ResponseWriter, request *http.Request) (parsedChatRequest, *requestProblem) {
	if problem := validateChatMediaType(request.Header.Get("Content-Type")); problem != nil {
		return parsedChatRequest{}, problem
	}
	if problem := validateChatContentEncoding(request.Header.Get("Content-Encoding")); problem != nil {
		return parsedChatRequest{}, problem
	}
	if request.ContentLength > maximumChatRequestBytes {
		return parsedChatRequest{}, requestTooLarge()
	}

	body := http.MaxBytesReader(writer, request.Body, maximumChatRequestBytes)
	defer func() { _ = body.Close() }()
	encoded, err := io.ReadAll(body)
	if err != nil {
		var maximumBytesError *http.MaxBytesError
		if errors.As(err, &maximumBytesError) {
			return parsedChatRequest{}, requestTooLarge()
		}
		return parsedChatRequest{}, newRequestProblem(
			http.StatusBadRequest, "INVALID_REQUEST_BODY", "The request body could not be read", "",
		)
	}
	if len(encoded) == 0 || !utf8.Valid(encoded) {
		return parsedChatRequest{}, invalidJSON()
	}
	if problem := validateJSONShape(encoded); problem != nil {
		return parsedChatRequest{}, problem
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		return parsedChatRequest{}, invalidJSON()
	}
	return parseChatRequestObject(object)
}

func validateChatMediaType(value string) *requestProblem {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return newRequestProblem(
			http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE",
			"Content-Type must be application/json",
			"",
		)
	}
	return nil
}

func validateChatContentEncoding(value string) *requestProblem {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "identity") {
		return nil
	}
	return newRequestProblem(
		http.StatusUnsupportedMediaType,
		"UNSUPPORTED_CONTENT_ENCODING",
		"Compressed request bodies are not supported",
		"",
	)
}

func validateJSONShape(encoded []byte) *requestProblem {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if problem := scanJSONValue(decoder, ""); problem != nil {
		return problem
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalidJSON()
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string) *requestProblem {
	token, err := decoder.Token()
	if err != nil {
		return invalidJSON()
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			key, isString := keyToken.(string)
			if keyErr != nil || !isString {
				return invalidJSON()
			}
			keyPath := appendSafeParameter(path, key)
			if _, exists := seen[key]; exists {
				return newRequestProblem(
					http.StatusBadRequest,
					"DUPLICATE_FIELD",
					"A JSON object contains a duplicate field",
					keyPath,
				)
			}
			seen[key] = struct{}{}
			if problem := scanJSONValue(decoder, keyPath); problem != nil {
				return problem
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return invalidJSON()
		}
	case '[':
		index := 0
		for decoder.More() {
			itemPath := fmt.Sprintf("%s[%d]", path, index)
			if problem := scanJSONValue(decoder, itemPath); problem != nil {
				return problem
			}
			index++
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return invalidJSON()
		}
	default:
		return invalidJSON()
	}
	return nil
}

func parseChatRequestObject(object map[string]json.RawMessage) (parsedChatRequest, *requestProblem) {
	if problem := rejectUnknownFields(object, "", []string{
		"model", "messages", "stream", "temperature", "top_p", "max_tokens",
		"max_completion_tokens", "stop", "tools", "tool_choice", "response_format", "user",
	}); problem != nil {
		return parsedChatRequest{}, problem
	}
	model, problem := requiredString(object, "model", "model")
	if problem != nil {
		return parsedChatRequest{}, problem
	}
	if !chatIdentifierPattern.MatchString(model) {
		return parsedChatRequest{}, invalidParameter("model")
	}
	messages, problem := parseMessages(object["messages"])
	if problem != nil {
		return parsedChatRequest{}, problem
	}
	parsed := parsedChatRequest{Model: model, Messages: messages}
	if raw, exists := object["stream"]; exists {
		if problem := decodeJSON(raw, &parsed.Stream, "stream"); problem != nil {
			return parsedChatRequest{}, problem
		}
	}
	if parsed.Temperature, problem = optionalFloat(object, "temperature", 0, 2, false); problem != nil {
		return parsedChatRequest{}, problem
	}
	if parsed.TopP, problem = optionalFloat(object, "top_p", 0, 1, true); problem != nil {
		return parsedChatRequest{}, problem
	}
	if _, legacy := object["max_tokens"]; legacy {
		if _, current := object["max_completion_tokens"]; current {
			return parsedChatRequest{}, newRequestProblem(
				http.StatusBadRequest,
				"CONFLICTING_PARAMETERS",
				"max_tokens and max_completion_tokens cannot be used together",
				"max_completion_tokens",
			)
		}
	}
	maximumField := "max_completion_tokens"
	if _, legacy := object["max_tokens"]; legacy {
		maximumField = "max_tokens"
	}
	if parsed.MaxCompletionTokens, problem = optionalPositiveInteger(object, maximumField); problem != nil {
		return parsedChatRequest{}, problem
	}
	if parsed.Stop, problem = parseStop(object["stop"]); problem != nil {
		return parsedChatRequest{}, problem
	}
	if parsed.Tools, problem = parseTools(object["tools"]); problem != nil {
		return parsedChatRequest{}, problem
	}
	if parsed.ToolChoice, problem = parseToolChoice(object["tool_choice"], parsed.Tools); problem != nil {
		return parsedChatRequest{}, problem
	}
	if parsed.ResponseFormat, problem = parseResponseFormat(object["response_format"]); problem != nil {
		return parsedChatRequest{}, problem
	}
	if parsed.User, problem = optionalString(object, "user", 256, true); problem != nil {
		return parsedChatRequest{}, problem
	}
	return parsed, nil
}

func parseMessages(raw json.RawMessage) ([]parsedChatMessage, *requestProblem) {
	if len(raw) == 0 {
		return nil, missingField("messages")
	}
	var values []json.RawMessage
	if problem := decodeJSON(raw, &values, "messages"); problem != nil {
		return nil, problem
	}
	if len(values) == 0 || len(values) > maximumChatMessages {
		return nil, invalidParameter("messages")
	}
	messages := make([]parsedChatMessage, len(values))
	for index, value := range values {
		field := fmt.Sprintf("messages[%d]", index)
		message, problem := parseMessage(value, field)
		if problem != nil {
			return nil, problem
		}
		messages[index] = message
	}
	return messages, nil
}

func parseMessage(raw json.RawMessage, field string) (parsedChatMessage, *requestProblem) {
	object, problem := decodeObject(raw, field)
	if problem != nil {
		return parsedChatMessage{}, problem
	}
	if problem := rejectUnknownFields(object, field, []string{"role", "content", "name", "tool_call_id", "tool_calls"}); problem != nil {
		return parsedChatMessage{}, problem
	}
	role, problem := requiredString(object, "role", field+".role")
	if problem != nil {
		return parsedChatMessage{}, problem
	}
	switch role {
	case "system", "developer", "user", "assistant", "tool":
	default:
		return parsedChatMessage{}, invalidParameter(field + ".role")
	}
	message := parsedChatMessage{Role: role}
	if message.Name, problem = optionalString(object, "name", 128, false); problem != nil {
		return parsedChatMessage{}, problemWithPath(problem, field+".name")
	}
	if message.Name != "" && !chatIdentifierPattern.MatchString(message.Name) {
		return parsedChatMessage{}, invalidParameter(field + ".name")
	}
	if message.ToolCallID, problem = optionalString(object, "tool_call_id", 256, false); problem != nil {
		return parsedChatMessage{}, problemWithPath(problem, field+".tool_call_id")
	}
	if message.ToolCallID != "" && !chatIdentifierPattern.MatchString(message.ToolCallID) {
		return parsedChatMessage{}, invalidParameter(field + ".tool_call_id")
	}
	if rawCalls, exists := object["tool_calls"]; exists {
		message.ToolCalls, problem = parseToolCalls(rawCalls, field+".tool_calls")
		if problem != nil {
			return parsedChatMessage{}, problem
		}
	}
	rawContent, hasContent := object["content"]
	if hasContent && !bytes.Equal(bytes.TrimSpace(rawContent), []byte("null")) {
		message.Content, problem = parseMessageContent(rawContent, field+".content")
		if problem != nil {
			return parsedChatMessage{}, problem
		}
	}
	if role == "assistant" {
		if len(message.Content) == 0 && len(message.ToolCalls) == 0 {
			return parsedChatMessage{}, missingField(field + ".content")
		}
	} else if !hasContent || len(message.Content) == 0 {
		return parsedChatMessage{}, missingField(field + ".content")
	}
	if role != "assistant" && len(message.ToolCalls) > 0 {
		return parsedChatMessage{}, invalidParameter(field + ".tool_calls")
	}
	if role == "tool" {
		if message.ToolCallID == "" {
			return parsedChatMessage{}, missingField(field + ".tool_call_id")
		}
	} else if message.ToolCallID != "" {
		return parsedChatMessage{}, invalidParameter(field + ".tool_call_id")
	}
	return message, nil
}

func parseMessageContent(raw json.RawMessage, field string) ([]parsedContentPart, *requestProblem) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" || len(text) > maximumTextBytes {
			return nil, invalidParameter(field)
		}
		return []parsedContentPart{{Kind: "text", Text: text}}, nil
	}
	var values []json.RawMessage
	if problem := decodeJSON(raw, &values, field); problem != nil {
		return nil, problem
	}
	if len(values) == 0 || len(values) > maximumChatMessages {
		return nil, invalidParameter(field)
	}
	parts := make([]parsedContentPart, len(values))
	for index, value := range values {
		partField := fmt.Sprintf("%s[%d]", field, index)
		part, problem := parseContentPart(value, partField)
		if problem != nil {
			return nil, problem
		}
		parts[index] = part
	}
	return parts, nil
}

func parseContentPart(raw json.RawMessage, field string) (parsedContentPart, *requestProblem) {
	object, problem := decodeObject(raw, field)
	if problem != nil {
		return parsedContentPart{}, problem
	}
	kind, problem := requiredString(object, "type", field+".type")
	if problem != nil {
		return parsedContentPart{}, problem
	}
	switch kind {
	case "text":
		if problem := rejectUnknownFields(object, field, []string{"type", "text"}); problem != nil {
			return parsedContentPart{}, problem
		}
		text, problem := requiredString(object, "text", field+".text")
		if problem != nil {
			return parsedContentPart{}, problem
		}
		if text == "" || len(text) > maximumTextBytes {
			return parsedContentPart{}, invalidParameter(field + ".text")
		}
		return parsedContentPart{Kind: kind, Text: text}, nil
	case "image_url":
		if problem := rejectUnknownFields(object, field, []string{"type", "image_url"}); problem != nil {
			return parsedContentPart{}, problem
		}
		imageObject, problem := decodeObject(object["image_url"], field+".image_url")
		if problem != nil {
			return parsedContentPart{}, problem
		}
		if problem := rejectUnknownFields(imageObject, field+".image_url", []string{"url", "detail"}); problem != nil {
			return parsedContentPart{}, problem
		}
		url, problem := requiredString(imageObject, "url", field+".image_url.url")
		if problem != nil {
			return parsedContentPart{}, problem
		}
		if url == "" || len(url) > 16*1024 {
			return parsedContentPart{}, invalidParameter(field + ".image_url.url")
		}
		detail, problem := optionalString(imageObject, "detail", 16, false)
		if problem != nil {
			return parsedContentPart{}, problemWithPath(problem, field+".image_url.detail")
		}
		if detail != "" && detail != "auto" && detail != "low" && detail != "high" {
			return parsedContentPart{}, invalidParameter(field + ".image_url.detail")
		}
		return parsedContentPart{Kind: kind, ImageURL: url, ImageDetail: detail}, nil
	default:
		return parsedContentPart{}, invalidParameter(field + ".type")
	}
}

func parseToolCalls(raw json.RawMessage, field string) ([]parsedToolCall, *requestProblem) {
	var values []json.RawMessage
	if problem := decodeJSON(raw, &values, field); problem != nil {
		return nil, problem
	}
	if len(values) == 0 || len(values) > maximumChatTools {
		return nil, invalidParameter(field)
	}
	calls := make([]parsedToolCall, len(values))
	for index, value := range values {
		callField := fmt.Sprintf("%s[%d]", field, index)
		object, problem := decodeObject(value, callField)
		if problem != nil {
			return nil, problem
		}
		if problem := rejectUnknownFields(object, callField, []string{"id", "type", "function"}); problem != nil {
			return nil, problem
		}
		identifier, problem := requiredString(object, "id", callField+".id")
		if problem != nil {
			return nil, problem
		}
		if !chatIdentifierPattern.MatchString(identifier) {
			return nil, invalidParameter(callField + ".id")
		}
		toolType, problem := requiredString(object, "type", callField+".type")
		if problem != nil {
			return nil, problem
		}
		if toolType != "function" {
			return nil, invalidParameter(callField + ".type")
		}
		functionField := callField + ".function"
		function, problem := decodeObject(object["function"], functionField)
		if problem != nil {
			return nil, problem
		}
		if problem := rejectUnknownFields(function, functionField, []string{"name", "arguments"}); problem != nil {
			return nil, problem
		}
		name, problem := requiredString(function, "name", functionField+".name")
		if problem != nil {
			return nil, problem
		}
		if !chatToolNamePattern.MatchString(name) {
			return nil, invalidParameter(functionField + ".name")
		}
		argumentsText, problem := requiredString(function, "arguments", functionField+".arguments")
		if problem != nil {
			return nil, problem
		}
		arguments := json.RawMessage(argumentsText)
		if problem := validateJSONObject(arguments, functionField+".arguments", maximumSchemaBytes); problem != nil {
			return nil, problem
		}
		calls[index] = parsedToolCall{ID: identifier, Name: name, Arguments: append(json.RawMessage(nil), arguments...)}
	}
	return calls, nil
}

func parseTools(raw json.RawMessage) ([]parsedChatTool, *requestProblem) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values []json.RawMessage
	if problem := decodeJSON(raw, &values, "tools"); problem != nil {
		return nil, problem
	}
	if len(values) == 0 || len(values) > maximumChatTools {
		return nil, invalidParameter("tools")
	}
	tools := make([]parsedChatTool, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		field := fmt.Sprintf("tools[%d]", index)
		object, problem := decodeObject(value, field)
		if problem != nil {
			return nil, problem
		}
		if problem := rejectUnknownFields(object, field, []string{"type", "function"}); problem != nil {
			return nil, problem
		}
		toolType, problem := requiredString(object, "type", field+".type")
		if problem != nil {
			return nil, problem
		}
		if toolType != "function" {
			return nil, invalidParameter(field + ".type")
		}
		functionField := field + ".function"
		function, problem := decodeObject(object["function"], functionField)
		if problem != nil {
			return nil, problem
		}
		if problem := rejectUnknownFields(function, functionField, []string{"name", "description", "parameters"}); problem != nil {
			return nil, problem
		}
		name, problem := requiredString(function, "name", functionField+".name")
		if problem != nil {
			return nil, problem
		}
		if !chatToolNamePattern.MatchString(name) {
			return nil, invalidParameter(functionField + ".name")
		}
		if _, exists := seen[name]; exists {
			return nil, invalidParameter(functionField + ".name")
		}
		seen[name] = struct{}{}
		description, problem := optionalString(function, "description", 4096, true)
		if problem != nil {
			return nil, problemWithPath(problem, functionField+".description")
		}
		parameters, exists := function["parameters"]
		if !exists {
			return nil, missingField(functionField + ".parameters")
		}
		if problem := validateJSONObject(parameters, functionField+".parameters", maximumSchemaBytes); problem != nil {
			return nil, problem
		}
		tools[index] = parsedChatTool{
			Name: name, Description: description,
			Parameters: append(json.RawMessage(nil), parameters...),
		}
	}
	return tools, nil
}

func parseToolChoice(raw json.RawMessage, tools []parsedChatTool) (*parsedToolChoice, *requestProblem) {
	if len(raw) == 0 {
		return nil, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		if mode != "none" && mode != "auto" && mode != "required" {
			return nil, invalidParameter("tool_choice")
		}
		if mode == "required" && len(tools) == 0 {
			return nil, invalidParameter("tool_choice")
		}
		return &parsedToolChoice{Mode: mode}, nil
	}
	object, problem := decodeObject(raw, "tool_choice")
	if problem != nil {
		return nil, problem
	}
	if problem := rejectUnknownFields(object, "tool_choice", []string{"type", "function"}); problem != nil {
		return nil, problem
	}
	toolType, problem := requiredString(object, "type", "tool_choice.type")
	if problem != nil {
		return nil, problem
	}
	if toolType != "function" {
		return nil, invalidParameter("tool_choice.type")
	}
	function, problem := decodeObject(object["function"], "tool_choice.function")
	if problem != nil {
		return nil, problem
	}
	if problem := rejectUnknownFields(function, "tool_choice.function", []string{"name"}); problem != nil {
		return nil, problem
	}
	name, problem := requiredString(function, "name", "tool_choice.function.name")
	if problem != nil {
		return nil, problem
	}
	if !chatToolNamePattern.MatchString(name) || !containsTool(tools, name) {
		return nil, invalidParameter("tool_choice.function.name")
	}
	return &parsedToolChoice{Mode: "named", Name: name}, nil
}

func parseResponseFormat(raw json.RawMessage) (*parsedResponseFormat, *requestProblem) {
	if len(raw) == 0 {
		return nil, nil
	}
	object, problem := decodeObject(raw, "response_format")
	if problem != nil {
		return nil, problem
	}
	formatType, problem := requiredString(object, "type", "response_format.type")
	if problem != nil {
		return nil, problem
	}
	switch formatType {
	case "text", "json_object":
		if problem := rejectUnknownFields(object, "response_format", []string{"type"}); problem != nil {
			return nil, problem
		}
		return &parsedResponseFormat{Type: formatType}, nil
	case "json_schema":
		if problem := rejectUnknownFields(object, "response_format", []string{"type", "json_schema"}); problem != nil {
			return nil, problem
		}
		schemaObject, problem := decodeObject(object["json_schema"], "response_format.json_schema")
		if problem != nil {
			return nil, problem
		}
		if problem := rejectUnknownFields(schemaObject, "response_format.json_schema", []string{"name", "description", "schema", "strict"}); problem != nil {
			return nil, problem
		}
		name, problem := requiredString(schemaObject, "name", "response_format.json_schema.name")
		if problem != nil {
			return nil, problem
		}
		if !chatToolNamePattern.MatchString(name) {
			return nil, invalidParameter("response_format.json_schema.name")
		}
		description, problem := optionalString(schemaObject, "description", 4096, true)
		if problem != nil {
			return nil, problemWithPath(problem, "response_format.json_schema.description")
		}
		schema, exists := schemaObject["schema"]
		if !exists {
			return nil, missingField("response_format.json_schema.schema")
		}
		if problem := validateJSONObject(schema, "response_format.json_schema.schema", maximumSchemaBytes); problem != nil {
			return nil, problem
		}
		var strict *bool
		if rawStrict, exists := schemaObject["strict"]; exists {
			value := false
			if problem := decodeJSON(rawStrict, &value, "response_format.json_schema.strict"); problem != nil {
				return nil, problem
			}
			strict = &value
		}
		return &parsedResponseFormat{
			Type: formatType, Name: name, Description: description,
			Schema: append(json.RawMessage(nil), schema...), Strict: strict,
		}, nil
	default:
		return nil, invalidParameter("response_format.type")
	}
}

func parseStop(raw json.RawMessage) ([]string, *requestProblem) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" || len(single) > 256 {
			return nil, invalidParameter("stop")
		}
		return []string{single}, nil
	}
	var stop []string
	if problem := decodeJSON(raw, &stop, "stop"); problem != nil {
		return nil, problem
	}
	if len(stop) == 0 || len(stop) > maximumChatStops {
		return nil, invalidParameter("stop")
	}
	seen := make(map[string]struct{}, len(stop))
	for index, value := range stop {
		if value == "" || len(value) > 256 {
			return nil, invalidParameter(fmt.Sprintf("stop[%d]", index))
		}
		if _, exists := seen[value]; exists {
			return nil, invalidParameter("stop")
		}
		seen[value] = struct{}{}
	}
	return stop, nil
}

func requiredString(object map[string]json.RawMessage, key, field string) (string, *requestProblem) {
	raw, exists := object[key]
	if !exists {
		return "", missingField(field)
	}
	var value string
	if problem := decodeJSON(raw, &value, field); problem != nil {
		return "", problem
	}
	return value, nil
}

func optionalString(object map[string]json.RawMessage, key string, maximum int, allowEmpty bool) (string, *requestProblem) {
	raw, exists := object[key]
	if !exists {
		return "", nil
	}
	var value string
	if problem := decodeJSON(raw, &value, key); problem != nil {
		return "", problem
	}
	if len(value) > maximum || (!allowEmpty && value == "") {
		return "", invalidParameter(key)
	}
	return value, nil
}

func optionalFloat(object map[string]json.RawMessage, field string, minimum, maximum float64, exclusiveMinimum bool) (*float64, *requestProblem) {
	raw, exists := object[field]
	if !exists {
		return nil, nil
	}
	var value float64
	if problem := decodeJSON(raw, &value, field); problem != nil {
		return nil, problem
	}
	if value < minimum || value > maximum || (exclusiveMinimum && value == minimum) {
		return nil, invalidParameter(field)
	}
	return &value, nil
}

func optionalPositiveInteger(object map[string]json.RawMessage, field string) (*int64, *requestProblem) {
	raw, exists := object[field]
	if !exists {
		return nil, nil
	}
	var value int64
	if problem := decodeJSON(raw, &value, field); problem != nil {
		return nil, problem
	}
	if value <= 0 {
		return nil, invalidParameter(field)
	}
	return &value, nil
}

func decodeObject(raw json.RawMessage, field string) (map[string]json.RawMessage, *requestProblem) {
	if len(raw) == 0 {
		return nil, missingField(field)
	}
	var object map[string]json.RawMessage
	if problem := decodeJSON(raw, &object, field); problem != nil {
		return nil, problem
	}
	if object == nil {
		return nil, invalidType(field)
	}
	return object, nil
}

func decodeJSON(raw json.RawMessage, target any, field string) *requestProblem {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return invalidType(field)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalidType(field)
	}
	return nil
}

func validateJSONObject(raw json.RawMessage, field string, maximum int) *requestProblem {
	if len(raw) == 0 {
		return missingField(field)
	}
	if len(raw) > maximum {
		return invalidParameter(field)
	}
	object, problem := decodeObject(raw, field)
	if problem != nil {
		return problem
	}
	if object == nil {
		return invalidType(field)
	}
	return nil
}

func rejectUnknownFields(object map[string]json.RawMessage, path string, allowed []string) *requestProblem {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}
	unknown := make([]string, 0)
	for field := range object {
		if _, exists := allowedSet[field]; !exists {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return newRequestProblem(
		http.StatusBadRequest,
		"UNSUPPORTED_FIELD",
		"The request contains an unsupported field",
		appendSafeParameter(path, unknown[0]),
	)
}

func appendSafeParameter(path, segment string) string {
	if !safeParameterSegmentPattern.MatchString(segment) {
		return path
	}
	if path == "" {
		return segment
	}
	return path + "." + segment
}

func containsTool(tools []parsedChatTool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func problemWithPath(problem *requestProblem, path string) *requestProblem {
	if problem == nil {
		return nil
	}
	problem.param = path
	return problem
}

func requestTooLarge() *requestProblem {
	return newRequestProblem(
		http.StatusRequestEntityTooLarge,
		"REQUEST_TOO_LARGE",
		"The request body exceeds the 1 MiB limit",
		"",
	)
}

func invalidJSON() *requestProblem {
	return newRequestProblem(
		http.StatusBadRequest,
		"INVALID_JSON",
		"The request body must contain exactly one valid JSON object",
		"",
	)
}

func missingField(field string) *requestProblem {
	return newRequestProblem(
		http.StatusBadRequest,
		"MISSING_REQUIRED_FIELD",
		"A required request field is missing",
		field,
	)
}

func invalidType(field string) *requestProblem {
	return newRequestProblem(
		http.StatusBadRequest,
		"INVALID_PARAMETER_TYPE",
		"A request field has an invalid JSON type",
		field,
	)
}

func invalidParameter(field string) *requestProblem {
	return newRequestProblem(
		http.StatusBadRequest,
		"INVALID_PARAMETER",
		"A request field has an invalid value",
		field,
	)
}

func newRequestProblem(status int, code, message, param string) *requestProblem {
	return &requestProblem{status: status, code: code, message: message, param: param}
}
