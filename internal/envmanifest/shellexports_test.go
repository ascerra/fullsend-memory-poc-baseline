package envmanifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExportedNames(t *testing.T) {
	tests := []struct {
		name    string
		scripts []string
		want    []string
	}{
		{"export with value", []string{"export FOO=bar"}, []string{"FOO"}},
		{"bare re-export", []string{"export OPENCODE_CONFIG_CONTENT"}, []string{"OPENCODE_CONFIG_CONTENT"}},
		{"several names in one export", []string{"export A=1 B C=3"}, []string{"A", "B", "C"}},
		{"export -n does not export", []string{"export -n FOO"}, []string{}},
		{"export -n removes an earlier export", []string{"export FOO=bar; export -n FOO"}, []string{}},
		{"export after export -n exports", []string{"export -n FOO; export FOO=bar"}, []string{"FOO"}},
		{"export -n with a value does not export", []string{"export -n FOO=bar BAR"}, []string{}},
		{"combined -n option un-exports", []string{"export A=1 B=2; export -pn A"}, []string{"B"}},
		{"other export flags are skipped", []string{"export -p FOO"}, []string{"FOO"}},
		{"double dash ends export options", []string{"export -- FOO"}, []string{"FOO"}},
		{"export -n inside sh -c", []string{`sh -c 'export FOO=1; export -n FOO'`}, []string{}},
		{"sh -c export -n keeps the caller's export", []string{`export FOO=1; sh -c 'export -n FOO'`}, []string{"FOO"}},
		{"export -n applies across scripts in order", []string{"export FOO=1", "export -n FOO"}, []string{}},
		{"export -n keeps a prefix assignment", []string{"FOO=1 cmd; export -n FOO"}, []string{"FOO"}},
		{"export -n keeps an env assignment", []string{"env FOO=1 cmd; export -n FOO"}, []string{"FOO"}},
		{"export -n keeps an sh -c child's export", []string{`sh -c 'export FOO=1; child'; export -n FOO`}, []string{"FOO"}},
		{"export -n keeps an sh -c child's prefix assignment", []string{`sh -c 'FOO=1 child'; export -n FOO`}, []string{"FOO"}},
		{"export -n in a subshell", []string{"export FOO=1; ( export -n FOO ); cmd"}, []string{"FOO"}},
		{"export -n in a pipeline", []string{"export FOO=1; export -n FOO | cat; cmd"}, []string{"FOO"}},
		{"export -n after &&", []string{"export FOO=1; false && export -n FOO"}, []string{"FOO"}},
		{"export -n after || on the next line", []string{"export FOO=1; true ||\nexport -n FOO"}, []string{"FOO"}},
		{"export -n before &&", []string{"export FOO=1; export -n FOO && cmd"}, []string{"FOO"}},
		{"export -n in the background", []string{"export FOO=1; export -n FOO &"}, []string{"FOO"}},
		{"export -n after a background job", []string{"export FOO=1; cmd & export -n FOO"}, []string{"FOO"}},
		{"export -n in a function body", []string{"export FOO=1; f() { export -n FOO; }"}, []string{"FOO"}},
		{"export -n in an if body", []string{"export FOO=1\nif cond; then\n  export -n FOO\nfi"}, []string{"FOO"}},
		{"export -n after a closed if", []string{"export FOO=1\nif cond; then\n  cmd\nfi\nexport -n FOO"}, []string{"FOO"}},
		{"export -n after a closed group", []string{"export FOO=1; ( cmd ) && { cmd; }; export -n FOO"}, []string{"FOO"}},
		{"case pattern does not close a subshell", []string{"export FOO=1; ( case x in a) cmd;; esac; export -n FOO )"}, []string{"FOO"}},
		{"export -n after a closed case", []string{"export FOO=1; case x in a) cmd;; esac; export -n FOO"}, []string{"FOO"}},
		{"export -n after a child keeps the name", []string{"export FOO=1; cmd; export -n FOO"}, []string{"FOO"}},
		{"export -n after a child keeps the name in sh -c", []string{`sh -c 'export FOO=1; cmd; export -n FOO'`}, []string{"FOO"}},
		{"export -n after a child in an earlier script", []string{"export FOO=1; cmd", "export -n FOO"}, []string{"FOO"}},
		{"export -n after only builtins removes", []string{"export FOO=1; echo hi; export -n FOO"}, []string{}},
		{"export -n in a same-line nested if", []string{"export FOO=1\nif a; then if b; then c; fi\nexport -n FOO\nfi"}, []string{"FOO"}},
		{"export -n in a nested for and if", []string{"export FOO=1; for f in a; do if t; then x; fi; export -n FOO; done"}, []string{"FOO"}},
		{"export -n in a here-document body", []string{"export FOO=1; read -r x <<EOF\nexport -n FOO\nEOF"}, []string{"FOO"}},
		{"export -n in a here-document before a child", []string{"export FOO=1; cat > s.sh <<EOF\nexport -n FOO\nEOF\n./child"}, []string{"FOO"}},
		{"export -fn keeps the variable", []string{"export FOO=1; export -fn FOO"}, []string{"FOO"}},
		{"plain assignment is shell-local", []string{"FOO=bar"}, []string{}},
		{"readonly assignment is shell-local", []string{"readonly FOO=bar"}, []string{}},
		{"dropping export drops the name", []string{"PI_OFFLINE=1 && export PI_TELEMETRY=0"}, []string{"PI_TELEMETRY"}},
		{"assignment text inside a quoted value", []string{`export FULLSEND_OUTPUT_FILE='NAME=value'`}, []string{"FULLSEND_OUTPUT_FILE"}},
		{"assignment text inside double quotes", []string{`echo "PHANTOM=1" && export REAL=1`}, []string{"REAL"}},
		{"quoted name is not an assignment", []string{`'FOO'=bar cmd`}, []string{}},
		{"prefix assignment reaches the command", []string{`HOME="$X" PATH="$Y" opencode run`}, []string{"HOME", "PATH"}},
		{"prefix assignment on a builtin does not", []string{"IFS= read -r line"}, []string{}},
		{"env command assignments", []string{"env -i A=1 B=2 node app.js"}, []string{"A", "B"}},
		{"exec passes prefix through", []string{"exec env A=1 node"}, []string{"A"}},
		{"sh -c script is parsed", []string{`sh -c 'export INNER=1 && run'`}, []string{"INNER"}},
		{"bash -lc script is parsed", []string{`bash -lc "export INNER=1"`}, []string{"INNER"}},
		{"command substitution does not leak", []string{`X="$(export SUB=1; echo "$SUB")" && export OUTER=1`}, []string{"OUTER"}},
		{"separators split commands", []string{"export A=1; export B=2 && export C=3 || export D=4 | cat"}, []string{"A", "B", "C", "D"}},
		{"newlines split commands", []string{"export A=1\nB=2\nexport C=3\n"}, []string{"A", "C"}},
		{"comments are ignored", []string{"# export NOPE=1\nexport YES=1"}, []string{"YES"}},
		{"brace group", []string{"{ export A=1; } && ( export B=2 )"}, []string{"A", "B"}},
		{"escaped quote stays in the value", []string{`export A="x\"B=1"`}, []string{"A"}},
		{"dedupes and sorts across scripts", []string{"export Z=1", "export A=1 Z=2"}, []string{"A", "Z"}},
		{"invalid names are ignored", []string{"export 1BAD=x -flag"}, []string{}},
		{"unterminated quote does not panic", []string{`export A="x`, `export B='y`, `X="$(unterminated`}, []string{"A", "B"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExportedNames(tt.scripts...))
		})
	}
}

func TestShellCommandsSimple(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   bool
	}{
		{"semicolons and newlines", "export A=1; cmd\nexport -n A # note", true},
		{"quoted operators are data", `echo "a && b | c (d) {e}"; echo 'f & g'`, true},
		{"braces inside a word", "echo ${A}x{y}", true},
		{"quoted reserved word", `'if' x`, true},
		{"reserved word as an argument", "echo if then fi", true},
		{"command substitution", `X="$(a | b)"; Y=$(c && d)`, true},
		{"and list", "a && b", false},
		{"or list", "a || b", false},
		{"pipeline", "a | b", false},
		{"background job", "a & b", false},
		{"subshell", "( a )", false},
		{"brace group", "{ a; }", false},
		{"if", "if a; then b; fi", false},
		{"same-line nested if", "if a; then if b; then c; fi\nd\nfi", false},
		{"for", "for f in a; do b; done", false},
		{"while", "while a; do b; done", false},
		{"negated command", "! a", false},
		{"here-document", "cat <<EOF\nexport -n A\nEOF", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, simple := shellCommands(tt.script)
			assert.Equal(t, tt.want, simple)
		})
	}
}
