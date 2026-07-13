package lib

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types/events"
)

func TestExtractIE(t *testing.T) {
	text := "Check [Google](https://google.com) or [!Untrusted](http://untrusted.com) or latex [x^2|100|50]<https://latex.codecogs.com/png.latex?x%5E2> and citation [](https://wikipedia.org)."
	clean, entities := extractIE(text)

	if !strings.Contains(clean, "{{WA_HYPERLINK_0}}") {
		t.Errorf("Expected WA_HYPERLINK_0 in clean text, got: %s", clean)
	}
	if !strings.Contains(clean, "{{WA_HYPERLINK_1}}") {
		t.Errorf("Expected WA_HYPERLINK_1 in clean text, got: %s", clean)
	}
	if !strings.Contains(clean, "{{WA_LATEX_0}}") {
		t.Errorf("Expected WA_LATEX_0 in clean text, got: %s", clean)
	}
	if !strings.Contains(clean, "{{WA_CITATION_0}}") {
		t.Errorf("Expected WA_CITATION_0 in clean text, got: %s", clean)
	}

	if len(entities) != 4 {
		t.Errorf("Expected 4 inline entities, got: %d", len(entities))
	}
}

func TestTokenizer(t *testing.T) {
	code := "const x = 123;\n// this is comment\nconsole.log(\"hello\");"
	blocks, unified := tokenizer(code, "javascript")

	if len(blocks) == 0 || len(unified) == 0 {
		t.Errorf("Expected tokens to be generated")
	}

	foundComment := false
	for _, u := range unified {
		if u["type"] == "COMMENT" {
			foundComment = true
		}
	}
	if !foundComment {
		t.Errorf("Expected to find COMMENT token type")
	}
}

func TestToTableMetadata(t *testing.T) {
	table := [][]string{
		{"Header 1", "Header 2"},
		{"Val 1", "Val 2"},
	}
	_, protoRows, unifiedRows := toTableMetadata(table)

	if len(protoRows) != 2 || len(unifiedRows) != 2 {
		t.Errorf("Expected 2 rows in both metadata formats")
	}
}

func TestBuilderBuild(t *testing.T) {
	builder := New(nil).
		SetTitle("Test Title").
		SetBody("Test Body").
		SetFooter("Test Footer").
		AddText("Simple paragraph").
		AddSuggest([]string{"Yes", "No"})

	msg := builder.Build(&events.Message{})
	if msg == nil {
		t.Fatal("Expected non-nil msg")
	}

	if msg.BotForwardedMessage == nil || msg.BotForwardedMessage.Message == nil || msg.BotForwardedMessage.Message.RichResponseMessage == nil {
		t.Fatal("Invalid protobuf hierarchy")
	}

	richMsg := msg.BotForwardedMessage.Message.RichResponseMessage
	if richMsg.UnifiedResponse == nil || len(richMsg.UnifiedResponse.Data) == 0 {
		t.Fatal("Expected unified response data to be populated")
	}

	decoded, err := base64.StdEncoding.DecodeString(string(richMsg.UnifiedResponse.Data))
	if err != nil {
		t.Fatalf("Failed to decode base64: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	sections, ok := payload["sections"].([]any)
	if !ok || len(sections) == 0 {
		t.Fatalf("Expected sections list to be populated")
	}
}
