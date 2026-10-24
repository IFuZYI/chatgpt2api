package protocol

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// 测试用的 1x1 PNG（data URL 与裸 base64 两种形态都会用到）。
const responseImageTestPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// collectStream 跑一遍 StreamImageResponse，把事件收成切片。
func collectStream(t *testing.T, outputs []ImageOutput, prompt, model string, inputImageTokens int, size any, quality string) ([]map[string]any, error) {
	t.Helper()
	channel := make(chan ImageOutput, len(outputs))
	for _, output := range outputs {
		channel <- output
	}
	close(channel)

	events := make([]map[string]any, 0, 8)
	err := StreamImageResponse(channel, prompt, model, inputImageTokens, size, quality, func(event map[string]any) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

// eventTypes 抽出事件类型序列，便于逐字比对顺序。
func eventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, stringOf(event["type"]))
	}
	return types
}

// tools 里的 image_generation 要被识别（含前后空白）。
func TestHasResponseImageGenerationTool(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{
			name: "tools 里显式声明",
			body: map[string]any{"tools": []any{map[string]any{"type": "image_generation"}}},
			want: true,
		},
		{
			name: "其他工具里混一个",
			body: map[string]any{"tools": []any{
				map[string]any{"type": "web_search"},
				map[string]any{"type": " image_generation "},
			}},
			want: true,
		},
		{
			name: "tool_choice 也算（Python 两条都看）",
			body: map[string]any{"tool_choice": map[string]any{"type": "image_generation"}},
			want: true,
		},
		{
			name: "没有图片工具",
			body: map[string]any{"tools": []any{map[string]any{"type": "web_search"}}},
			want: false,
		},
		{
			name: "tools 不是数组",
			body: map[string]any{"tools": "image_generation"},
			want: false,
		},
		{
			name: "空 body",
			body: map[string]any{},
			want: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := HasResponseImageGenerationTool(testCase.body); got != testCase.want {
				t.Errorf("HasResponseImageGenerationTool = %v, want %v", got, testCase.want)
			}
		})
	}
}

// ResponseImageTool 取第一个 image_generation；没有时返回 nil。
func TestResponseImageTool(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "image_generation", "size": "1024x1024", "quality": "high"},
	}}
	tool := ResponseImageTool(body)
	if tool == nil {
		t.Fatal("ResponseImageTool = nil, want the image_generation tool")
	}
	if tool["size"] != "1024x1024" || tool["quality"] != "high" {
		t.Errorf("tool = %#v", tool)
	}
	if got := ResponseImageTool(map[string]any{}); got != nil {
		t.Errorf("missing tool = %#v, want nil", got)
	}
}

// prompt 抽取：字符串 / 消息对象 / 内容块数组 / 消息数组四种形态。
func TestExtractResponsePrompt(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{name: "字符串", input: "  画一只猫  ", want: "画一只猫"},
		{
			name:  "单条消息对象",
			input: map[string]any{"role": "user", "content": "画一只猫"},
			want:  "画一只猫",
		},
		{
			name:  "非 user 消息对象返回空",
			input: map[string]any{"role": "assistant", "content": "画一只猫"},
			want:  "",
		},
		{
			name: "内容块数组里的 input_text",
			input: []any{
				map[string]any{"type": "input_text", "text": "画一只猫"},
			},
			want: "画一只猫",
		},
		{
			name: "消息数组：多条 user 用换行拼",
			input: []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "画一只猫"},
				}},
				map[string]any{"role": "assistant", "content": "好的"},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "背景用星空"},
				}},
			},
			want: "画一只猫\n背景用星空",
		},
		{
			name:  "空数组",
			input: []any{},
			want:  "",
		},
		{
			name:  "nil",
			input: nil,
			want:  "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ExtractResponsePrompt(testCase.input); got != testCase.want {
				t.Errorf("ExtractResponsePrompt = %q, want %q", got, testCase.want)
			}
		})
	}
}

// 参考图抽取：input_image 独立形态 + 嵌套在 content 里的形态。
func TestExtractResponseImage(t *testing.T) {
	t.Run("input_image 类型", func(t *testing.T) {
		image, err := ExtractResponseImage([]any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + responseImageTestPNG},
		})
		if err != nil {
			t.Fatalf("ExtractResponseImage: %v", err)
		}
		if image == nil {
			t.Fatal("image = nil, want the decoded reference image")
		}
		if image.MIMEType != "image/png" {
			t.Errorf("mime = %q", image.MIMEType)
		}
		if payload, _ := base64.StdEncoding.DecodeString(responseImageTestPNG); string(image.Data) != string(payload) {
			t.Errorf("data mismatch: got %d bytes", len(image.Data))
		}
	})

	t.Run("嵌套 content 里的 image_url", func(t *testing.T) {
		image, err := ExtractResponseImage([]any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "把它改成蓝色"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + responseImageTestPNG},
			}},
		})
		if err != nil {
			t.Fatalf("ExtractResponseImage: %v", err)
		}
		if image == nil {
			t.Fatal("image = nil, want the nested reference image")
		}
	})

	t.Run("单条消息对象", func(t *testing.T) {
		image, err := ExtractResponseImage(map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + responseImageTestPNG},
			},
		})
		if err != nil || image == nil {
			t.Fatalf("image = %v, err = %v", image, err)
		}
	})

	t.Run("没有图", func(t *testing.T) {
		image, err := ExtractResponseImage([]any{
			map[string]any{"role": "user", "content": "画一只猫"},
		})
		if err != nil || image != nil {
			t.Fatalf("image = %v, err = %v, want nil/nil", image, err)
		}
	})

	// 逆序取第一张：后出现的消息里的图优先（Python 的 reversed 循环）。
	t.Run("逆序优先取最新的一张", func(t *testing.T) {
		oldImage := base64.StdEncoding.EncodeToString([]byte("old-image"))
		newImage := base64.StdEncoding.EncodeToString([]byte("new-image"))
		image, err := ExtractResponseImage([]any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + oldImage},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + newImage},
			}},
		})
		if err != nil || image == nil {
			t.Fatalf("image = %v, err = %v", image, err)
		}
		want, _ := base64.StdEncoding.DecodeString(newImage)
		if string(image.Data) != string(want) {
			t.Errorf("data = %q, want the newest image", image.Data)
		}
	})
}

// image_output_items 的形状：只保留 b64、id 连续、revised_prompt 回落 prompt。
func TestImageOutputItems(t *testing.T) {
	data := []map[string]any{
		{"b64_json": "AAA"},
		{"b64_json": "  "},
		{"b64_json": "BBB", "revised_prompt": "改好的提示词"},
		{"b64_json": "CCC", "revised_prompt": "   "},
	}
	items := ImageOutputItems("原始提示词", data)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (条目按 b64_json 过滤)", len(items))
	}
	wantIDs := []string{"ig_1", "ig_2", "ig_3"}
	wantResults := []string{"AAA", "BBB", "CCC"}
	wantPrompts := []string{"原始提示词", "改好的提示词", "原始提示词"}
	for index, item := range items {
		if item["id"] != wantIDs[index] {
			t.Errorf("item %d id = %#v, want %q", index, item["id"], wantIDs[index])
		}
		if item["type"] != "image_generation_call" {
			t.Errorf("item %d type = %#v", index, item["type"])
		}
		if item["status"] != "completed" {
			t.Errorf("item %d status = %#v", index, item["status"])
		}
		if item["result"] != wantResults[index] {
			t.Errorf("item %d result = %#v", index, item["result"])
		}
		if item["revised_prompt"] != wantPrompts[index] {
			t.Errorf("item %d revised_prompt = %#v, want %q", index, item["revised_prompt"], wantPrompts[index])
		}
	}
	if empty := ImageOutputItems("p", nil); len(empty) != 0 {
		t.Errorf("empty data = %#v", empty)
	}
}

// 图片结果的事件顺序：created → N × output_item.done → completed。
//
// 逐字断言**没有** output_item.added 与 partial_image 事件：
// Python 的图片分支不产出它们。
func TestStreamImageResponseImageEventOrder(t *testing.T) {
	events, err := collectStream(t, []ImageOutput{
		{Kind: ImageKindProgress, Text: "正在绘制"},
		{Kind: ImageKindResult, Data: []map[string]any{
			{"b64_json": "AAA"},
			{"b64_json": "BBB"},
		}},
	}, "画一只猫", "gpt-image-2", 0, "1024x1024", "auto")
	if err != nil {
		t.Fatalf("StreamImageResponse: %v", err)
	}

	want := []string{
		"response.created",
		"response.output_item.done",
		"response.output_item.done",
		"response.completed",
	}
	got := eventTypes(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}

	// response.created 的形状。
	created := events[0]["response"].(map[string]any)
	if created["status"] != "in_progress" {
		t.Errorf("created status = %#v", created["status"])
	}
	if output, ok := created["output"].([]any); !ok || len(output) != 0 {
		t.Errorf("created output = %#v, want empty", created["output"])
	}
	if created["object"] != "response" || created["parallel_tool_calls"] != false {
		t.Errorf("created = %#v", created)
	}

	// output_index 从 0 递增。
	for index, event := range events[1:3] {
		if event["output_index"] != index {
			t.Errorf("event %d output_index = %#v, want %d", index, event["output_index"], index)
		}
	}

	// completed 带完整 output 与 usage。
	completed := events[3]["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Errorf("completed status = %#v", completed["status"])
	}
	output, _ := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output = %d, want 2", len(output))
	}
	if _, ok := completed["usage"]; !ok {
		t.Error("completed must carry usage")
	}

	// 同一响应里 id 一致。
	createdID, _ := created["id"].(string)
	if !strings.HasPrefix(createdID, "resp_") {
		t.Errorf("response id = %q", createdID)
	}
	if completed["id"] != createdID {
		t.Errorf("completed id = %#v, want %q", completed["id"], createdID)
	}
	// uuid4().hex 是 32 位无连字符：resp_ + 32。
	if len(strings.TrimPrefix(createdID, "resp_")) != 32 {
		t.Errorf("response id = %q, want resp_ + 32 hex chars", createdID)
	}
}

// 文本回复的事件顺序：created → delta → done → output_item.done → completed。
func TestStreamImageResponseMessageEventOrder(t *testing.T) {
	events, err := collectStream(t, []ImageOutput{
		{Kind: ImageKindMessage, Text: "这个内容画不了"},
	}, "画一只猫", "gpt-image-2", 0, nil, "auto")
	if err != nil {
		t.Fatalf("StreamImageResponse: %v", err)
	}

	want := []string{
		"response.created",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	got := eventTypes(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}

	itemID := events[1]["item_id"]
	if events[2]["item_id"] != itemID {
		t.Errorf("done item_id = %#v, want %#v", events[2]["item_id"], itemID)
	}
	if events[1]["delta"] != "这个内容画不了" || events[2]["text"] != "这个内容画不了" {
		t.Errorf("delta/text = %#v / %#v", events[1]["delta"], events[2]["text"])
	}

	item := events[3]["item"].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" || item["status"] != "completed" {
		t.Errorf("item = %#v", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "这个内容画不了" {
		t.Errorf("content = %#v", content)
	}
	if _, ok := content["annotations"].([]any); !ok {
		t.Errorf("content annotations = %#v, want an array", content["annotations"])
	}

	completed := events[4]["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output = %d, want 1", len(output))
	}
	usage := completed["usage"].(map[string]any)
	if details, ok := usage["output_tokens_details"].(map[string]any); !ok || details["text_tokens"] == 0 {
		t.Errorf("usage = %#v, want text output tokens", usage)
	}
}

// 一条图都没有时整条流报 image generation failed（Python 的 RuntimeError）。
func TestStreamImageResponseFailsWithoutOutput(t *testing.T) {
	events, err := collectStream(t, []ImageOutput{
		{Kind: ImageKindProgress, Text: "正在绘制"},
		{Kind: ImageKindResult, Data: []map[string]any{{"b64_json": "  "}}},
	}, "画一只猫", "gpt-image-2", 0, nil, "auto")
	if err == nil {
		t.Fatalf("err = nil, want image generation failed (events: %v)", eventTypes(events))
	}
	if !errors.Is(err, ErrImageResponseGenerationFailed) {
		t.Errorf("err = %v, want ErrImageResponseGenerationFailed", err)
	}
	// created 在读到 output 之前就发了：这条路径一定已有 1 条事件。
	if len(events) != 1 {
		t.Errorf("events = %v, want only response.created", eventTypes(events))
	}
}

// emit 失败时立即停止，并把错误原样返回（SSE 写失败不该继续烧额度）。
func TestStreamImageResponseStopsWhenEmitFails(t *testing.T) {
	outputs := make(chan ImageOutput, 2)
	outputs <- ImageOutput{Kind: ImageKindResult, Data: []map[string]any{{"b64_json": "AAA"}}}
	outputs <- ImageOutput{Kind: ImageKindResult, Data: []map[string]any{{"b64_json": "BBB"}}}
	close(outputs)

	writeErrSentinel := errors.New("client went away")
	count := 0
	err := StreamImageResponse(outputs, "画一只猫", "gpt-image-2", 0, nil, "auto", func(event map[string]any) error {
		count++
		if count == 2 {
			return writeErrSentinel
		}
		return nil
	})
	if !errors.Is(err, writeErrSentinel) {
		t.Fatalf("err = %v, want the emit error", err)
	}
	if count != 2 {
		t.Errorf("emit called %d times, want 2 (stop at the failure)", count)
	}
}

// 空 result（只有空 b64）要继续往后找，不能提前结束。
func TestStreamImageResponseSkipsEmptyResult(t *testing.T) {
	events, err := collectStream(t, []ImageOutput{
		{Kind: ImageKindResult, Data: []map[string]any{{"b64_json": ""}}},
		{Kind: ImageKindResult, Data: []map[string]any{{"b64_json": "AAA"}}},
	}, "画一只猫", "gpt-image-2", 0, nil, "auto")
	if err != nil {
		t.Fatalf("StreamImageResponse: %v", err)
	}
	want := []string{"response.created", "response.output_item.done", "response.completed"}
	got := eventTypes(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	output := events[2]["response"].(map[string]any)["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["result"] != "AAA" {
		t.Errorf("output = %#v", output)
	}
}

// usage 的 input_image_tokens 要进 input_tokens_details.image_tokens。
func TestStreamImageResponseUsageCarriesImageTokens(t *testing.T) {
	events, err := collectStream(t, []ImageOutput{
		{Kind: ImageKindResult, Data: []map[string]any{{"b64_json": "AAA"}}},
	}, "画一只猫", "gpt-image-2", 1659, "1024x1024", "auto")
	if err != nil {
		t.Fatalf("StreamImageResponse: %v", err)
	}
	usage := events[len(events)-1]["response"].(map[string]any)["usage"].(map[string]any)
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if inputDetails["image_tokens"] != 1659 {
		t.Errorf("image_tokens = %#v, want 1659", inputDetails["image_tokens"])
	}
	outputDetails := usage["output_tokens_details"].(map[string]any)
	if outputDetails["image_tokens"] == 0 {
		t.Errorf("output image_tokens = %#v, want a non-zero image count", outputDetails["image_tokens"])
	}
	if outputDetails["text_tokens"] != 0 {
		t.Errorf("output text_tokens = %#v, want 0 (image_usage)", outputDetails["text_tokens"])
	}
}

// CollectResponse 取最后一个 completed 的 response。
func TestCollectResponse(t *testing.T) {
	response, err := CollectResponse([]map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_a"}},
		{"type": "response.completed", "response": map[string]any{"id": "resp_b"}},
	})
	if err != nil {
		t.Fatalf("CollectResponse: %v", err)
	}
	if response["id"] != "resp_b" {
		t.Errorf("id = %#v, want resp_b", response["id"])
	}
	if _, err := CollectResponse([]map[string]any{{"type": "response.created"}}); err == nil {
		t.Fatal("want response generation failed")
	}
}

// input_image_tokens 只统计 input 里真正的图。
//
// 1x1 PNG 落到 patch 预算下界，算出来是 2（与 Python 的
// count_image_content_tokens 同一路径）；关键是**不是 0** —— 没走到计数
// 就会退化成 0，usage 里的 image_tokens 也跟着丢。
func TestCountResponseImageInputTokens(t *testing.T) {
	t.Run("消息数组里的图", func(t *testing.T) {
		input := []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "把它改成蓝色"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + responseImageTestPNG},
		}}}
		if tokens := CountResponseImageInputTokens(input, "gpt-image-2"); tokens != 2 {
			t.Errorf("tokens = %d, want 2", tokens)
		}
	})

	t.Run("扁平内容块数组", func(t *testing.T) {
		input := []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + responseImageTestPNG},
		}
		if tokens := CountResponseImageInputTokens(input, "gpt-image-2"); tokens != 2 {
			t.Errorf("tokens = %d, want 2", tokens)
		}
	})

	t.Run("单条消息对象", func(t *testing.T) {
		input := map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + responseImageTestPNG},
		}}
		if tokens := CountResponseImageInputTokens(input, "gpt-image-2"); tokens != 2 {
			t.Errorf("tokens = %d, want 2", tokens)
		}
	})

	t.Run("纯文本为 0", func(t *testing.T) {
		input := []any{map[string]any{"role": "user", "content": "画一只猫"}}
		if tokens := CountResponseImageInputTokens(input, "gpt-image-2"); tokens != 0 {
			t.Errorf("text-only tokens = %d, want 0", tokens)
		}
	})
}
