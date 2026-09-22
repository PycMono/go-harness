package providers

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/PycMono/go-harness/pi/ai"
	pierrors "github.com/PycMono/go-harness/pi/error"
	"github.com/PycMono/go-harness/pi/schema"
	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"
)

const (
	streamResultText         = "先读一下这个文件"
	streamResultToolCallID   = "call-stream-result"
	streamResultToolName     = "read_file"
	streamResultToolArgs     = `{"path":"a.go"}`
	streamResultInputTokens  = 21
	streamResultOutputTokens = 13
)

// TestStreamResultReturnsNilInterfaceWhenEmpty 钉住 Result() 的 nil 契约：没有结果时
// 交出来的必须是 nil 接口，而不是装着 (*schema.AssistantMessage)(nil) 的非 nil 接口。
// 后者类型断言照样成功，调用方的 message == nil 判定却会静默走错分支——"空流"与
// "有结果的流"从此分不出来。同时钉住另一条可区分性：没结果且没失败是 (nil, nil)，
// 失败流是 (nil, 错误)，两者不能混成一个。
func TestStreamResultReturnsNilInterfaceWhenEmpty(t *testing.T) {
	cases := []struct {
		name    string
		stream  ai.Stream
		wantErr bool
	}{
		{
			name:   "没有结果也没有错误",
			stream: &failedStream{},
		},
		{
			name:    "失败且没有结果",
			stream:  newFailedStream(pierrors.ErrAIGeneration.Wrap(errors.New("上游连接断开"))),
			wantErr: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for testCase.stream.Next() {
			}

			message, err := testCase.stream.Result()
			if message != nil {
				t.Fatalf("没有结果时 message 的动态类型 = %T，想要 nil 接口", message)
			}
			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("未失败流的错误 = %v，想要 nil", err)
				}
				return
			}
			if !pierrors.ErrAIGeneration.Match(err) {
				t.Fatalf("失败流的错误码 = %d，想要 %d（err = %v）",
					pierrors.CodeOf(err), pierrors.ErrAIGeneration.Code(), err)
			}
		})
	}
}

// TestOpenAIStreamResultCarriesAssistantMessage 钉住迁移后的动态类型：Provider 产出
// 的具体值是 *schema.AssistantMessage，Result() 断言得回来，且 Usage、FinishReason、
// ToolCalls、Content 一个不少。这里直接驱动 finish()——它才是把 SDK 响应落成消息的
// 那段生产代码，不需要网络也不需要 client。
func TestOpenAIStreamResultCarriesAssistantMessage(t *testing.T) {
	stream := &openAIStream{usageSeen: true}
	stream.accumulator.ChatCompletion.Choices = []openaisdk.ChatCompletionChoice{{
		FinishReason: "tool_calls",
		Message: openaisdk.ChatCompletionMessage{
			Content: streamResultText,
			ToolCalls: []openaisdk.ChatCompletionMessageToolCallUnion{{
				ID:   streamResultToolCallID,
				Type: "function",
				Function: openaisdk.ChatCompletionMessageFunctionToolCallFunction{
					Name:      streamResultToolName,
					Arguments: streamResultToolArgs,
				},
			}},
		},
	}}
	stream.accumulator.ChatCompletion.Usage = openaisdk.CompletionUsage{
		PromptTokens:     streamResultInputTokens,
		CompletionTokens: streamResultOutputTokens,
	}

	if err := stream.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	message, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	assistant, ok := message.(*schema.AssistantMessage)
	if !ok {
		t.Fatalf("Result() 的动态类型 = %T，想要 *schema.AssistantMessage", message)
	}
	if assistant == nil {
		t.Fatal("Result() 交出来的是 typed nil 指针")
	}

	if got := assistant.Role(); got != schema.RoleAssistant {
		t.Fatalf("Role() = %q，想要 %q", got, schema.RoleAssistant)
	}
	text, err := assistant.Content.Text()
	if err != nil {
		t.Fatalf("Content.Text: %v", err)
	}
	if text != streamResultText {
		t.Fatalf("Content 文本 = %q，想要 %q", text, streamResultText)
	}
	if assistant.FinishReason != schema.FinishReasonToolUse {
		t.Fatalf("FinishReason = %q，想要 %q", assistant.FinishReason, schema.FinishReasonToolUse)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("ToolCalls 条数 = %d，想要 1", len(assistant.ToolCalls))
	}
	if call := assistant.ToolCalls[0]; call.ID != streamResultToolCallID ||
		call.Name != streamResultToolName || string(call.Arguments) != streamResultToolArgs {
		t.Fatalf("ToolCall = {%q, %q, %s}，想要 {%q, %q, %s}",
			call.ID, call.Name, call.Arguments,
			streamResultToolCallID, streamResultToolName, streamResultToolArgs)
	}
	if assistant.Usage == nil {
		t.Fatal("Usage 丢失")
	}
	if assistant.Usage.InputTokens != streamResultInputTokens ||
		assistant.Usage.OutputTokens != streamResultOutputTokens {
		t.Fatalf("Usage = {输入 %d, 输出 %d}，想要 {输入 %d, 输出 %d}",
			assistant.Usage.InputTokens, assistant.Usage.OutputTokens,
			streamResultInputTokens, streamResultOutputTokens)
	}
}

// TestAnthropicStreamResultCarriesAssistantMessage 与 OpenAI 侧对称：Anthropic 的
// finish() 同样落成 *schema.AssistantMessage，工具调用与用量归一化（输入 = 缓存写入
// + 缓存读取 + 原始输入）保持原样。
func TestAnthropicStreamResultCarriesAssistantMessage(t *testing.T) {
	stream := &anthropicStream{message: anthropicsdk.Message{
		Content: []anthropicsdk.ContentBlockUnion{
			{Type: "text", Text: streamResultText},
			{
				Type: "tool_use", ID: streamResultToolCallID, Name: streamResultToolName,
				Input: json.RawMessage(streamResultToolArgs),
			},
		},
		StopReason: anthropicsdk.StopReasonToolUse,
		Usage: anthropicsdk.Usage{
			InputTokens:              streamResultInputTokens,
			OutputTokens:             streamResultOutputTokens,
			CacheReadInputTokens:     3,
			CacheCreationInputTokens: 5,
		},
	}}

	if err := stream.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	message, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	assistant, ok := message.(*schema.AssistantMessage)
	if !ok {
		t.Fatalf("Result() 的动态类型 = %T，想要 *schema.AssistantMessage", message)
	}
	if assistant == nil {
		t.Fatal("Result() 交出来的是 typed nil 指针")
	}

	text, err := assistant.Content.Text()
	if err != nil {
		t.Fatalf("Content.Text: %v", err)
	}
	if text != streamResultText {
		t.Fatalf("Content 文本 = %q，想要 %q", text, streamResultText)
	}
	if assistant.FinishReason != schema.FinishReasonToolUse {
		t.Fatalf("FinishReason = %q，想要 %q", assistant.FinishReason, schema.FinishReasonToolUse)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("ToolCalls 条数 = %d，想要 1", len(assistant.ToolCalls))
	}
	if call := assistant.ToolCalls[0]; call.ID != streamResultToolCallID ||
		call.Name != streamResultToolName || string(call.Arguments) != streamResultToolArgs {
		t.Fatalf("ToolCall = {%q, %q, %s}，想要 {%q, %q, %s}",
			call.ID, call.Name, call.Arguments,
			streamResultToolCallID, streamResultToolName, streamResultToolArgs)
	}
	if assistant.Usage == nil {
		t.Fatal("Usage 丢失")
	}
	if got := assistant.Usage.InputTokens; got != streamResultInputTokens+3+5 {
		t.Fatalf("InputTokens = %d，想要 %d", got, streamResultInputTokens+3+5)
	}
	if got := assistant.Usage.OutputTokens; got != streamResultOutputTokens {
		t.Fatalf("OutputTokens = %d，想要 %d", got, streamResultOutputTokens)
	}
	if got := assistant.Usage.CacheReadTokens; got != 3 {
		t.Fatalf("CacheReadTokens = %d，想要 3", got)
	}
	if got := assistant.Usage.CacheWriteTokens; got != 5 {
		t.Fatalf("CacheWriteTokens = %d，想要 5", got)
	}
}
