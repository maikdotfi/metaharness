package tools

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/skills"
)

// SkillArgs is the tool's input: which skill to load, plus free-form arguments
// for the skill's instructions to act on.
type SkillArgs struct {
	Name string `json:"skill" description:"Name of the skill to run, exactly as it appears in the available skills list."`
	Args string `json:"args,omitempty" description:"Optional arguments for the skill."`
}

// NewSkill builds the skill tool over a fixed catalog. The catalog is captured
// by the closure and rendered into the tool description, so the list the model
// is shown and the lookup the tool performs cannot drift.
func NewSkill(available ...skills.Skill) agent.Tool {
	byName := make(map[string]skills.Skill, len(available))
	names := make([]string, 0, len(available))
	for _, s := range available {
		byName[s.Name] = s
		names = append(names, s.Name)
	}

	meta := agent.ToolMeta{
		Name:        "skill",
		Description: skillDescription + "\n\n" + renderCatalog(available),
	}

	return agent.AdaptFunc(meta,
		func(ctx context.Context, ec *agent.ExecCtx, args SkillArgs) (agent.ToolResult, error) {
			s, ok := byName[args.Name]
			if !ok {
				// Name the valid options so the model can self-correct on the
				// next turn instead of retrying blind.
				return agent.ToolResult{
					Content: fmt.Sprintf("unknown skill %q; available skills: %s", args.Name, strings.Join(names, ", ")),
					IsError: true,
				}, nil
			}
			// The wrapper matches the shape promised in the tool description,
			// and its name attribute is what lets a later turn recognize the
			// skill as already loaded.
			var b strings.Builder
			fmt.Fprintf(&b, "<skill_content name=%q>\n%s\n", s.Name, strings.TrimRight(s.Instructions, "\n"))
			if args.Args != "" {
				fmt.Fprintf(&b, "\nArguments passed to the skill: %s\n", args.Args)
			}
			b.WriteString("</skill_content>")
			return agent.ToolResult{Content: b.String()}, nil
		},
	)
}

const skillDescription = `Execute a skill within the conversation. Skills provide specialized capabilities and domain knowledge; the result of this call is the skill's full instructions — follow them for the task at hand.

When the user asks you to perform a task, check if one of the available skills matches. If one does, this is a BLOCKING REQUIREMENT: invoke this tool BEFORE generating any other response about the task, and never mention a skill without actually calling this tool. Set ` + "`args`" + ` to pass optional arguments to the skill.

Only invoke a skill listed below, or one the user explicitly names in their message. Never guess or invent a skill name; if nothing matches, do not call this tool.

A loaded skill appears in the conversation as an XML block:

<skill_content name="skill-name">
[the skill's instructions]
</skill_content>

If a <skill_content> block for a skill is already present in the current conversation, that skill is ALREADY loaded — follow its instructions directly instead of calling this tool again.`

// catalogTmpl renders the available-skills block of the tool description.
var catalogTmpl = template.Must(template.New("catalog").Parse(`<available_skills>
{{- range .}}
  <skill>
    <name>{{.Name}}</name>
    <description>{{.Description}}</description>
  </skill>
{{- end}}
</available_skills>`))

func renderCatalog(available []skills.Skill) string {
	var b strings.Builder
	if err := catalogTmpl.Execute(&b, available); err != nil {
		// The template and the fields it references are fixed at compile time,
		// so a failure here is a programmer error, not bad runtime input.
		panic(err)
	}
	return b.String()
}
