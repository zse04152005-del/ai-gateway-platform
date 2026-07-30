// Package mockprovider implements a deterministic, local-only provider protocol simulator.
package mockprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumRequestBody = 1 << 20
	defaultDelayMS     = 100
	maximumDelayMS     = 5000

	// ScenarioHeader selects a deterministic behavior without changing an OpenAI request body.
	ScenarioHeader = "X-Mock-Scenario"
)

type scenario string

const (
	scenarioNormal         scenario = "normal"
	scenarioSSE            scenario = "sse"
	scenarioFixedUsage     scenario = "fixed-usage"
	scenarioDelay          scenario = "delay"
	scenarioRateLimit      scenario = "rate-limit"
	scenarioServerError    scenario = "server-error"
	scenarioDisconnect     scenario = "disconnect"
	scenarioMalformedChunk scenario = "malformed-chunk"
	scenarioCachedUsage    scenario = "cached-usage"
	scenarioToolCall       scenario = "tool-call"
)

var supportedScenarios = map[scenario]struct{}{
	scenarioNormal: {}, scenarioSSE: {}, scenarioFixedUsage: {}, scenarioDelay: {},
	scenarioRateLimit: {}, scenarioServerError: {}, scenarioDisconnect: {},
	scenarioMalformedChunk: {}, scenarioCachedUsage: {}, scenarioToolCall: {},
}

type chatRequest struct {
	Model        string        `json:"model"`
	Messages     []chatMessage `json:"messages"`
	Stream       bool          `json:"stream,omitempty"`
	MockScenario string        `json:"mock_scenario,omitempty"`
	MockDelayMS  *int          `json:"mock_delay_ms,omitempty"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type requestFailure struct {
	status  int
	code    string
	message string
	param   string
}

func (failure *requestFailure) Error() string {
	return failure.message
}

func validateChatRequest(request chatRequest) *requestFailure {
	if !validText(request.Model, 1, 200) {
		return invalidRequest("invalid_model", "model must be 1-200 trimmed characters", "model")
	}
	if len(request.Messages) < 1 || len(request.Messages) > 256 {
		return invalidRequest("invalid_messages", "messages must contain 1-256 entries", "messages")
	}
	for index, message := range request.Messages {
		if !oneOf(message.Role, "system", "developer", "user", "assistant", "tool") {
			return invalidRequest(
				"invalid_message_role",
				fmt.Sprintf("messages[%d].role is unsupported", index),
				"messages",
			)
		}
		if len(message.Content) == 0 {
			return invalidRequest(
				"invalid_message_content",
				fmt.Sprintf("messages[%d].content is required", index),
				"messages",
			)
		}
	}
	return nil
}

func resolveScenario(httpRequest *http.Request, request chatRequest) (scenario, int, *requestFailure) {
	headerValue, failure := oneScenarioValue(httpRequest.Header.Values(ScenarioHeader), "header")
	if failure != nil {
		return "", 0, failure
	}
	queryValue, failure := oneScenarioValue(httpRequest.URL.Query()["scenario"], "query")
	if failure != nil {
		return "", 0, failure
	}
	bodyValue := request.MockScenario
	if bodyValue != strings.TrimSpace(bodyValue) {
		return "", 0, invalidRequest("invalid_mock_scenario", "mock_scenario must be trimmed", "mock_scenario")
	}

	selected := ""
	for _, candidate := range []string{headerValue, queryValue, bodyValue} {
		if candidate == "" {
			continue
		}
		if selected != "" && selected != candidate {
			return "", 0, invalidRequest(
				"ambiguous_mock_scenario",
				"scenario selectors must agree when more than one is provided",
				"mock_scenario",
			)
		}
		selected = candidate
	}
	if selected == "" {
		if request.Stream {
			selected = string(scenarioSSE)
		} else {
			selected = string(scenarioNormal)
		}
	}
	selectedScenario := scenario(selected)
	if _, ok := supportedScenarios[selectedScenario]; !ok {
		return "", 0, invalidRequest(
			"unknown_mock_scenario",
			"mock_scenario is not supported",
			"mock_scenario",
		)
	}

	delayMS := 0
	if selectedScenario == scenarioDelay {
		delayMS = defaultDelayMS
		if request.MockDelayMS != nil {
			delayMS = *request.MockDelayMS
		}
		if delayMS < 1 || delayMS > maximumDelayMS {
			return "", 0, invalidRequest(
				"invalid_mock_delay",
				"mock_delay_ms must be between 1 and 5000",
				"mock_delay_ms",
			)
		}
	} else if request.MockDelayMS != nil {
		return "", 0, invalidRequest(
			"unexpected_mock_delay",
			"mock_delay_ms is only valid for the delay scenario",
			"mock_delay_ms",
		)
	}
	return selectedScenario, delayMS, nil
}

func oneScenarioValue(values []string, source string) (string, *requestFailure) {
	if len(values) > 1 {
		return "", invalidRequest(
			"ambiguous_mock_scenario",
			fmt.Sprintf("%s scenario selector must have exactly one value", source),
			"mock_scenario",
		)
	}
	if len(values) == 0 {
		return "", nil
	}
	value := values[0]
	if value != strings.TrimSpace(value) || value == "" || strings.Contains(value, ",") {
		return "", invalidRequest(
			"invalid_mock_scenario",
			fmt.Sprintf("%s scenario selector is invalid", source),
			"mock_scenario",
		)
	}
	return value, nil
}

func invalidRequest(code, message, param string) *requestFailure {
	return &requestFailure{status: http.StatusBadRequest, code: code, message: message, param: param}
}

func validText(value string, minimum, maximum int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && value == strings.TrimSpace(value) && length >= minimum && length <= maximum &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
