package integrations

import "strings"

// parseAntigravityCommand recognizes the literal command written by this
// integration, including an explicit per-profile state file. Shell pipelines,
// expansions and unknown options are left for the user instead of dropping
// them during an upgrade. No command is executed while parsing.
func parseAntigravityCommand(command string) (binary, stateFile string, ok bool) {
	words, valid := antigravityLiteralWords(command)
	if !valid || len(words) < 3 || words[0] == "" || words[1] != "antigravity" || words[2] != "statusline" {
		return "", "", false
	}
	switch {
	case len(words) == 3:
	case len(words) == 5 && words[3] == "--state-file" && words[4] != "":
		stateFile = words[4]
	case len(words) == 4 && strings.HasPrefix(words[3], "--state-file="):
		stateFile = strings.TrimPrefix(words[3], "--state-file=")
		if stateFile == "" {
			return "", "", false
		}
	default:
		return "", "", false
	}
	return words[0], stateFile, true
}

func quoteAntigravityArgument(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(value) + `"`
}

// A deliberately small literal-only shell lexer, sufficient for paths quoted
// by the installer and manually supplied --state-file arguments. Reject shell
// evaluation instead of guessing its result or changing its meaning.
func antigravityLiteralWords(command string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '\n' || c == '\r' || c == 0 {
			return nil, false
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if c == '\\' {
			if i+1 == len(command) {
				return nil, false
			}
			next := command[i+1]
			if next == '\n' || next == '\r' {
				return nil, false
			}
			if quote == '"' && !strings.ContainsRune("\\\"$`", rune(next)) {
				word.WriteByte(c)
			} else {
				i++
				word.WriteByte(next)
			}
			started = true
			continue
		}
		if c == '$' || c == '`' {
			return nil, false
		}
		if quote == '"' {
			if c == '"' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		case ';', '&', '|', '<', '>', '(', ')', '*', '?', '[', ']', '{', '}', '~', '#':
			return nil, false
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, true
}
