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
//     "export NAME"; "export -n NAME" can remove the export again (see
//     below), so scripts are read in order;
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
// "export -n" only clears the export attribute, so a name a child already
// received stays reported. A child receives a name through a prefix
// assignment, through "env NAME=", or through an "sh -c" child's own
// export. It also receives every exported name when any non-builtin
// command runs after the export. Because the tokenizer does not model
// scope, "export -n" removes a name only in a script that is a plain list
// of commands (see shellCommands). It removes nothing in a script with
// any group, operator, compound command or here-document. "export -fn"
// names functions and never removes a variable. On any doubt the name
// stays reported: over-reporting is the safe direction for an env
// coverage check.
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
// assignments, "env NAME=", whatever an "sh -c" child exported, and the
// exported names at the time a non-builtin command ran); nothing the
// script does afterwards takes them back. exported holds the
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
	cmds, simple := shellCommands(script)
	for _, words := range cmds {
		collectFromCommand(words, simple, st, depth)
	}
}

// collectFromCommand records the names one simple command exports. simple
// is shellCommands' verdict on the enclosing script; "export -n" removes
// names only when it is true.
func collectFromCommand(words []shellWord, simple bool, st *exportState, depth int) {
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
		if cmd != "export" {
			// The command may start a child process, which inherits every
			// name exported so far; a later "export -n" cannot take that
			// back.
			for name := range st.exported {
				st.delivered[name] = true
			}
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
			case simple:
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
				collectFromCommand(args[j:], simple, st, depth)
				return
			}
			st.delivered[name] = true
		}
	case "exec", "command", "nohup":
		collectFromCommand(args, simple, st, depth)
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

// shellReserved are the reserved words that open, continue or close a
// compound command (or prefix a pipeline) when they start a command.
var shellReserved = map[string]bool{
	"!": true, "case": true, "coproc": true, "do": true, "done": true,
	"elif": true, "else": true, "esac": true, "fi": true, "for": true,
	"function": true, "if": true, "select": true, "then": true, "time": true,
	"until": true, "while": true,
}

// shellCommands splits script into simple commands, each a list of words
// with quotes removed. Command substitutions are kept as opaque word text.
//
// simple reports that script is a plain list: its commands are separated
// only by ";" and newlines, and none starts with a reserved word, so each
// one runs unconditionally, in order, in the current shell. Any ( ) { }
// group, & && || | |& operator, compound command or here-document makes
// it false, because the tokenizer splits those but does not model their
// scope.
func shellCommands(script string) (cmds [][]shellWord, simple bool) {
	var (
		words      []shellWord
		cur        strings.Builder
		inWord     bool
		nameQuoted bool
		sawEq      bool
	)
	simple = true
	endWord := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), nameQuoted: nameQuoted})
		}
		cur.Reset()
		inWord, nameQuoted, sawEq = false, false, false
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			if !words[0].nameQuoted && shellReserved[words[0].text] {
				simple = false
			}
			cmds = append(cmds, words)
		}
		words = nil
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
			endCmd()
		case c == '&' || c == '|' || c == '(' || c == ')':
			simple = false
			endCmd()
		case (c == '{' || c == '}') && !inWord:
			simple = false
			endCmd()
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
			endCmd()
		default:
			// A here-document body would be read as live commands.
			if strings.HasPrefix(script[i:], "<<") {
				simple = false
			}
			inWord = true
			if c == '=' {
				sawEq = true
			}
			cur.WriteByte(c)
		}
	}
	endCmd()
	return cmds, simple
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
