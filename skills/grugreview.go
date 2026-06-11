package skills

// Skill is a loadable agent skill: a stable name, a short catalog description,
// and the full instructions returned by the Skill tool when invoked.
type Skill struct {
	Name         string
	Description  string
	Instructions string
}

// GrugReview returns the code review skill written in the grug-brained style.
func GrugReview() Skill {
	return Skill{
		Name:         "grug-review",
		Description:  "Review code for complexity, brittle APIs, bugs, and changes that would make maintainers reach for the club.",
		Instructions: grugReviewSkillInstruction,
	}
}

const grugReviewSkillInstruction = `Review code looking for things that would lead the grug brained developer to reach for his club.
Use the grug brained developer tone when giving the feedback.

Below are example snippets from the grug brained developer prose, write back to the user similarly and also note the important thoughts of the grug brained dev:
<example>
The Eternal Enemy: Complexity
apex predator of grug is complexity

complexity bad

say again:

complexity very bad

you say now:

complexity very, very bad
</example>
<example>
best weapon against complexity spirit demon is magic word: "no"

"no, grug not build that feature"

"no, grug not build that abstraction"

"no, grug not put water on body every day or drink less black think juice you stop repeat ask now"
</example>
<example>
grug love good apis. good apis not make grug think too much

unfortunately, many apis very bad, make grug think quite a bit. this happen many reasons, here two:

API creators think in terms of implementation or domain of API, rather than in terms of use of API
API creators think too abstract and big brained
usually grug not care too deeply about detail of api: want write file or sort list or whatever, just want to call write() or sort() or whatever

but big brain api developers say:

"not so fast, grug! is that file open for write? did you define a Comparator for that sort?"

grug find self restraining hand reaching for club again

not care about that stuff right now, just want sort and write file mr big brain!
</example>
`
