package tools

import (
	"strings"
	"testing"

	"github.com/maikdotfi/metaharness/skills"
	"github.com/maikdotfi/metaharness/testutils"
)

// TestSkill invokes a known skill through the full dispatch path and checks the
// result is the skill's instructions and the caller's arguments wrapped in the
// <skill_content> block the tool description promises, and that the catalog the
// tool was built with shows up in its definition.
func TestSkill(t *testing.T) {
	ec, _ := testutils.NewExecCtx(t)
	pirate := skills.Skill{
		Name:         "pirate_greeting",
		Description:  "Greet the user in pirate speak.",
		Instructions: "Always open with 'Ahoy!' and close with 'Yarr.'",
	}
	tool := NewSkill(pirate)

	wantCatalog := `<available_skills>
  <skill>
    <name>pirate_greeting</name>
    <description>Greet the user in pirate speak.</description>
  </skill>
</available_skills>`
	def := tool.Definition()
	if !strings.Contains(def.Description, wantCatalog) {
		t.Errorf("tool description missing catalog block %q, got %q", wantCatalog, def.Description)
	}
	t.Logf("\nDescription: \n\n%s", def.Description)

	res := testutils.CallTool(t, ec, tool, SkillArgs{
		Name: "pirate_greeting",
		Args: "the user is named Bob",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := `<skill_content name="pirate_greeting">
Always open with 'Ahoy!' and close with 'Yarr.'

Arguments passed to the skill: the user is named Bob
</skill_content>`
	if res.Content != want {
		t.Errorf("Content = %q, want %q", res.Content, want)
	}
	t.Logf("\nContent: \n\n%s", res.Content)
}

// TestSkillGrugReview runs the real grug-review skill from the skills package
// through the same dispatch path, checking it shows up in the catalog and that
// invoking it returns its full instructions in the <skill_content> wrapper.
func TestSkillGrugReview(t *testing.T) {
	ec, _ := testutils.NewExecCtx(t)
	grug := skills.GrugReview()
	tool := NewSkill(grug)

	wantCatalog := `<available_skills>
  <skill>
    <name>grug-review</name>
    <description>` + grug.Description + `</description>
  </skill>
</available_skills>`
	def := tool.Definition()
	if !strings.Contains(def.Description, wantCatalog) {
		t.Errorf("tool description missing catalog block %q, got %q", wantCatalog, def.Description)
	}

	res := testutils.CallTool(t, ec, tool, SkillArgs{
		Name: "grug-review",
		Args: "review the diff on the current branch",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := `<skill_content name="grug-review">
` + strings.TrimRight(grug.Instructions, "\n") + `

Arguments passed to the skill: review the diff on the current branch
</skill_content>`
	if res.Content != want {
		t.Errorf("Content = %q, want %q", res.Content, want)
	}
	t.Logf("\nContent: \n\n%s", res.Content)
}
