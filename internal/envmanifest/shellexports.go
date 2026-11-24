package envmanifest

import (
	"regexp"
	"slices"
	"strings"
)

// envNameRe is the POSIX shell variable-name grammar.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExportedNames returns the sorted, distinct variable names the shell
// scripts place in a child process's environment. It recognizes three forms:
//
//   - export statements, both "export NAME=value" and the bare re-export
//     "export NAME"; "export -n NAME" removes the export again, so scripts
//     are read in order and a name un-exported last is not reported;
//   - prefix assignments on a command ("NAME=value cmd" and
//     "env NAME=value cmd"), which the command's process inherits;
//   - the same forms inside the script argument of "sh -c" or "bash -c",
//     parsed recursively.
//
// A plain assignment with no command ("NAME=value" alone, or "readonly
// NAME=value") is shell-local and is not reported, nor is a prefix
// assignment on a shell builtin ("IFS= read"). Assignment-shaped text
// inside a quoted value is data, not a binding, and a command
// substitution's exports stay in its subshell. The parser is a tokenizer,
// not a shell: it handles single and double quotes, backslash escapes,
// $(...) nesting, comments, and the ; && || | & ( ) { } newline
// separators, which is the subset the runner's emitters generate.
//
// "export -n" only clears the export attribute. A name a child already
// received through a prefix assignment, "env NAME=", or an "sh -c" child's
// own export stays reported. Because the tokenizer does not model scope,
// "export -n" removes a name only when it runs unconditionally in the
// current shell (see shellCommand.scoped). "export -fn" names functions
// and never removes a variable. On any doubt the name stays reported:
// over-reporting is the safe direction for an env coverage check.
func ExportedNames(scripts ...string) []string {
	st := newExportState()
	for _, script := range scripts {
		collectExportedNames(script, st, 0)
	}
	names := make([]string, 0, len(st.delivered)+len(st.exported))
	for name := range st.delivered {
		names = append(names, name)
	}
	for name := range st.exported {
		if !st.delivered[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// exportState keeps the two ways a name reaches a child's environment
// apart. delivered holds names a child process already received (prefix
// assignments, "env NAME=", and whatever an "sh -c" child exported);
// nothing the script does afterwards takes them back. exported holds the
// names carrying the shell's export attribute, which "export -n" clears.
type exportState struct {
	delivered map[string]bool
	exported  map[string]bool
}

func newExportState() *exportState {
	return &exportState{delivered: map[string]bool{}, exported: map[string]bool{}}
}

// shellBuiltins are commands that run inside the shell itself, so a prefix
// assignment on them ("IFS= read -r line") scopes to that one builtin and
// never reaches a child process's environment.
var shellBuiltins = map[string]bool{
	":": true, "[": true, "break": true, "cd": true, "continue": true,
	"echo": true, "eval": true, "exit": true, "false": true, "printf": true,
	"read": true, "return": true, "set": true, "shift": true, "test": true,
	"true": true, "unset": true,
}

// maxShellDepth bounds sh -c recursion so a malformed input cannot loop.
const maxShellDepth = 8

// shellWord is one word of a simple command after quote removal.
// nameQuoted records that a quote opened before the word's first '=': sh
// treats 'A'=b as a command word, not an assignment.
type shellWord struct {
	text       string
	nameQuoted bool
}

// assignmentName returns NAME when w is an unquoted-name NAME=value word,
// or "" otherwise.
func (w shellWord) assignmentName() string {
	if w.nameQuoted {
		return ""
	}
	name, _, ok := strings.Cut(w.text, "=")
	if !ok || !envNameRe.MatchString(name) {
		return ""
	}
	return name
}

func collectExportedNames(script string, st *exportState, depth int) {
	if depth > maxShellDepth {
		return
	}
	for _, c := range shellCommands(script) {
		collectFromCommand(c.words, c.scoped, st, depth)
	}
}

// collectFromCommand records the names one simple command exports. scoped
// is the command's shellCommand.scoped flag.
func collectFromCommand(words []shellWord, scoped bool, st *exportState, depth int) {
	// Leading NAME=value words are prefix assignments when a command
	// follows them, and shell-local assignments when nothing does.
	i := 0
	for i < len(words) && words[i].assignmentName() != "" {
		i++
	}
	if i == len(words) {
		return
	}
	cmd, args := words[i].text, words[i+1:]
	if !shellBuiltins[cmd] {
		for _, w := range words[:i] {
			st.delivered[w.assignmentName()] = true
		}
	}
	switch cmd {
	case "export":
		// Leading option words end at the first non-option word or "--".
		// -n (alone or combined, as in -pn) removes the export attribute,
		// so the listed names leave the environment instead of joining it.
		// With -f the names are functions, so -fn leaves variables alone
		// and is handled like -f.
		unexport, functions := false, false
		for len(args) > 0 && strings.HasPrefix(args[0].text, "-") {
			opt := args[0].text
			args = args[1:]
			if opt == "--" {
				break
			}
			unexport = unexport || strings.Contains(opt[1:], "n")
			functions = functions || strings.Contains(opt[1:], "f")
		}
		unexport = unexport && !functions
		for _, a := range args {
			if strings.HasPrefix(a.text, "-") {
				continue
			}
			name := a.assignmentName()
			if name == "" && envNameRe.MatchString(a.text) {
				name = a.text
			}
			if name == "" {
				continue
			}
			switch {
			case !unexport:
				st.exported[name] = true
			case !scoped:
				delete(st.exported, name)
			}
		}
	case "env":
		for j, a := range args {
			if strings.HasPrefix(a.text, "-") {
				continue
			}
			name := a.assignmentName()
			if name == "" {
				collectFromCommand(args[j:], scoped, st, depth)
				return
			}
			st.delivered[name] = true
		}
	case "exec", "command", "nohup":
		collectFromCommand(args, scoped, st, depth)
	case "sh", "bash", "/bin/sh", "/bin/bash":
		for j, a := range args {
			if strings.HasPrefix(a.text, "-") && strings.Contains(a.text, "c") && j+1 < len(args) {
				// The child shell has its own export state: an "export -n"
				// inside it cannot un-export a name the caller already
				// placed in the child's environment, and the caller's later
				// "export -n" cannot take back what the child exported.
				inner := newExportState()
				collectExportedNames(args[j+1].text, inner, depth+1)
				for name := range inner.delivered {
					st.delivered[name] = true
				}
				for name := range inner.exported {
					st.delivered[name] = true
				}
				return
			}
		}
	}
}

// shellCommand is one simple command: its words with quotes removed.
// scoped records that the command might not run unconditionally in the
// current shell: it sits inside ( ) or { } (subshells, groups, function
// bodies) or an if/while/until/for/case/select body, it is joined to a
// neighbor by && || | |&, or it runs in the background with &. The
// tokenizer cannot tell those apart, so it marks them all.
type shellCommand struct {
	words  []shellWord
	scoped bool
}

// joinsNext and joinsPrev are the operators that make the command before
// or after them scoped. A command after a background "&" still runs in
// the current shell.
var (
	joinsNext = map[string]bool{"&": true, "&&": true, "||": true, "|": true, "|&": true}
	joinsPrev = map[string]bool{"&&": true, "||": true, "|": true, "|&": true}
)

// shellCommands splits script into simple commands. Command substitutions
// are kept as opaque word text.
func shellCommands(script string) []shellCommand {
	var (
		cmds       []shellCommand
		words      []shellWord
		cur        strings.Builder
		inWord     bool
		nameQuoted bool
		sawEq      bool
		// groupDepth counts open ( and {. kwDepth counts open compound
		// commands; caseDepth counts open case commands, whose pattern
		// ")" closes no group. Any nonzero depth, including one an
		// unbalanced script drives negative, marks commands scoped.
		groupDepth, kwDepth, caseDepth int
		// prevOp is the operator that joined the previous command.
		prevOp string
	)
	endWord := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), nameQuoted: nameQuoted})
		}
		cur.Reset()
		inWord, nameQuoted, sawEq = false, false, false
	}
	// endCmd ends the current command at op: a control operator, ";" for
	// a plain command end, or "" at a group bracket.
	endCmd := func(op string) {
		endWord()
		if len(words) == 0 {
			// A blank line or the gap at a bracket keeps the operator that
			// joins the next command to the previous one.
			if op != ";" && op != "" {
				prevOp = op
			}
			return
		}
		first := words[0].text
		if words[0].nameQuoted {
			first = ""
		}
		switch first {
		case "case":
			caseDepth++
			kwDepth++
		case "if", "while", "until", "for", "select":
			kwDepth++
		}
		scoped := groupDepth != 0 || kwDepth != 0 || joinsPrev[prevOp] || joinsNext[op]
		switch first {
		case "esac":
			caseDepth--
			kwDepth--
		case "fi", "done":
			kwDepth--
		}
		cmds = append(cmds, shellCommand{words: words, scoped: scoped})
		words = nil
		prevOp = op
	}
	quote := func() {
		inWord = true
		if !sawEq {
			nameQuoted = true
		}
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case c == '\'':
			quote()
			j := strings.IndexByte(script[i+1:], '\'')
			if j < 0 {
				cur.WriteString(script[i+1:])
				i = len(script)
				break
			}
			cur.WriteString(script[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			quote()
			for i++; i < len(script) && script[i] != '"'; i++ {
				switch {
				case script[i] == '\\' && i+1 < len(script) && strings.IndexByte("\"\\$`\n", script[i+1]) >= 0:
					i++
				case strings.HasPrefix(script[i:], "$("):
					end := skipSubstitution(script, i+2)
					cur.WriteString(script[i:end])
					i = end - 1
					continue
				}
				cur.WriteByte(script[i])
			}
		case strings.HasPrefix(script[i:], "$("):
			// A command substitution runs in a subshell: its exports never
			// reach the caller, so its text is kept as opaque word data.
			inWord = true
			end := skipSubstitution(script, i+2)
			cur.WriteString(script[i:end])
			i = end - 1
		case c == '\\' && i+1 < len(script):
			inWord = true
			i++
			if script[i] != '\n' {
				cur.WriteByte(script[i])
			}
		case c == ' ' || c == '\t':
			endWord()
		case c == '\n' || c == ';':
			endCmd(";")
		case c == '&' || c == '|':
			op := string(c)
			if i+1 < len(script) && (script[i+1] == c || (c == '|' && script[i+1] == '&')) {
				op += string(script[i+1])
				i++
			}
			endCmd(op)
		case c == '(' || (c == '{' && !inWord):
			endCmd("")
			groupDepth++
		case c == ')' || (c == '}' && !inWord):
			endCmd("")
			if c == '}' || caseDepth == 0 {
				groupDepth--
			}
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
			endCmd(";")
		default:
			inWord = true
			if c == '=' {
				sawEq = true
			}
			cur.WriteByte(c)
		}
	}
	endCmd(";")
	return cmds
}

// skipSubstitution returns the index just past the ")" closing the command
// substitution whose body starts at i (just after "$("), honoring nested
// quotes, escapes, and parentheses. An unterminated substitution consumes
// the rest of the script.
func skipSubstitution(s string, i int) int {
	depth := 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return len(s)
			}
			i += j + 2
			continue
		case '"':
			i = skipDoubleQuoted(s, i+1)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return len(s)
}

// skipDoubleQuoted returns the index just past the '"' closing the
// double-quoted string whose body starts at i, descending into command
// substitutions so their own quotes do not end it.
func skipDoubleQuoted(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == '\\':
			i += 2
		case s[i] == '"':
			return i + 1
		case strings.HasPrefix(s[i:], "$("):
			i = skipSubstitution(s, i+2)
		default:
			i++
		}
	}
	return len(s)
}
