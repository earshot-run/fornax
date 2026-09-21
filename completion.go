package main

// `fornax completion <zsh|bash|fish>` prints a shell completion script to
// stdout. Command names are baked in at build time; model ids are resolved
// at completion time by calling the hidden `__complete_models` command.

import (
	"fmt"
	"strings"
)

// Every subcommand — keep in sync with the switch in main.go.
const completionCommands = "list pull rm clean doctor ask chat see hear say draw embed compare test bench judge run ps connect show talk record search version upgrade mcp rerank completion"

// Subcommands whose first positional argument is a model id.
const completionModelCommands = "pull rm ask chat see hear say draw embed compare test bench judge run connect show talk record rerank"

const zshCompletion = `#compdef fornax
# fornax completion for zsh — eval "$(fornax completion zsh)", or save to a
# file named _fornax anywhere on $fpath.

_fornax() {
	local -a commands models shells
	commands=(@COMMANDS@)
	case $CURRENT in
	2)
		_describe 'fornax command' commands
		;;
	3)
		case ${words[2]} in
		@MODELS_ALT@)
			models=(${(f)"$(fornax __complete_models 2>/dev/null)"})
			_describe 'model' models
			;;
		completion)
			shells=(zsh bash fish)
			_describe 'shell' shells
			;;
		esac
		;;
	esac
}

_fornax "$@"
`

const bashCompletion = `# fornax completion for bash — eval "$(fornax completion bash)", or save to
# /etc/bash_completion.d/fornax.

_fornax() {
	local cur
	cur="${COMP_WORDS[COMP_CWORD]}"
	case $COMP_CWORD in
	1)
		COMPREPLY=($(compgen -W "@COMMANDS@" -- "$cur"))
		;;
	2)
		case ${COMP_WORDS[1]} in
		@MODELS_ALT@)
			COMPREPLY=($(compgen -W "$(fornax __complete_models 2>/dev/null)" -- "$cur"))
			;;
		completion)
			COMPREPLY=($(compgen -W "zsh bash fish" -- "$cur"))
			;;
		esac
		;;
	esac
}
complete -o default -F _fornax fornax
`

const fishCompletion = `# fornax completion for fish — save to ~/.config/fish/completions/fornax.fish:
#   fornax completion fish > ~/.config/fish/completions/fornax.fish

function __fornax_second_arg --description 'completing the argument right after the subcommand'
    set -l tokens (commandline -opc)
    test (count $tokens) -eq 2
end

complete -c fornax -f -n '__fish_use_subcommand' -a '@COMMANDS@'
complete -c fornax -f -n '__fornax_second_arg; and __fish_seen_subcommand_from @MODELS@' -a '(fornax __complete_models 2>/dev/null)'
complete -c fornax -f -n '__fornax_second_arg; and __fish_seen_subcommand_from completion' -a 'zsh bash fish'
`

func cmdCompletion(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: fornax completion <zsh|bash|fish>")
	}
	var script string
	switch args[0] {
	case "zsh":
		script = zshCompletion
	case "bash":
		script = bashCompletion
	case "fish":
		script = fishCompletion
	default:
		return fmt.Errorf("usage: fornax completion <zsh|bash|fish>")
	}
	script = strings.NewReplacer(
		"@COMMANDS@", completionCommands,
		"@MODELS@", completionModelCommands,
		"@MODELS_ALT@", strings.ReplaceAll(completionModelCommands, " ", "|"),
	).Replace(script)
	fmt.Print(script)
	return nil
}

// Hidden helper the completion scripts call — every known model id, one per
// line. Wired in main.go as `__complete_models`; a failure just yields no
// completions.
func cmdCompleteModels() error {
	for _, spec := range allSpecs(home()) {
		fmt.Println(spec.id)
	}
	return nil
}
