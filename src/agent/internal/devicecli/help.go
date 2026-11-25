package devicecli

import "strings"

// Keep the root listing and subcommand help on the same command reference.
var commandUsage = []string{
	"look [--dir DIRECTORY] [--region X1 Y1 X2 Y2]",
	"wait [milliseconds]",
	"home", "back", "enter",
	"tap X Y", "double-tap X Y", "long-press X Y",
	"swipe X1 Y1 X2 Y2 [milliseconds]", "drag X1 Y1 X2 Y2",
	"drag-start X Y", "drag-release X Y", "type TEXT", "key KEY [KEY ...]",
	"app type TEXT", "app clipboard read", "app clipboard write TEXT",
	"app calendar query --from TIME --to TIME",
	"app calendar create --title TITLE --start-at TIME [--end-at TIME] [--all-day] [--location TEXT] [--notes TEXT] [--alarm-minutes-before N]",
	"app calendar delete --event-id ID",
	"app contacts query [--query TEXT] [--limit N]",
	"app contacts create --name NAME [--phone-number NUMBER] [--email ADDRESS] [--organization TEXT] [--notes TEXT]",
	"app contacts update --contact-id ID [--name NAME] [--phone-number NUMBER] [--email ADDRESS] [--organization TEXT] [--notes TEXT]",
	"app notification send --title TITLE [--body TEXT] [--schedule-at TIME] [--sound[=true|false]] [--badge N]",
	"open APP", "url URL", "quick ACTION", "scroll DY", "move X Y",
}

var Usage = "Usage: device-cli [--url URL] COMMAND\n  " + strings.Join(commandUsage, "\n  ") + `
Run device-cli COMMAND --help for a focused command reference.
App commands use the available companion routes. Calendar and scheduled notification times use RFC3339 with a timezone.
Repeat --phone-number or --email to supply multiple values.
For chords, use key meta+a or key meta a. Use key '+' for a literal plus.
Coordinates are integers from 0 to 1000. Quote text as one shell argument.
look prints the saved JPEG path. wait pauses for 0–30000 ms (default: 2000 ms).
Exit status 0 means success; failures exit nonzero and report errors on stderr.
Stdout is empty for acknowledgements, prints a single string verbatim or a single
non-string value as JSON, and uses a JSON object for multiple result fields.`

var helpNotes = map[string]string{
	"app":                "App commands use the available companion routes. Use --help on a group or action to narrow this list.",
	"app type":           "Paste into the focused field using the companion App clipboard. This does not verify the final field contents.",
	"app clipboard":      "Read prints the raw text (empty output is valid). Write an empty string to clear the clipboard.",
	"app clipboard read": "Print only the clipboard text, without adding a newline.",
	"app calendar":       "Use RFC3339 times with a timezone.",
	"app contacts":       "Repeat --phone-number or --email to supply multiple values.",
	"app notification":   "Use an RFC3339 time with a timezone for --schedule-at. Omit it to send immediately; sound defaults to false.",
	"look":               "Save a JPEG (default directory /tmp) and print its path. Region coordinates are normalized to 0–1000.",
	"wait":               "Fixed delay: 0–30000 ms, default 2000 ms. In an Agent shell, request a screenshot at shell exit.",
	"tap":                "Coordinates are normalized to 0–1000. Wait 50 ms after a successful tap.",
	"key":                "Use key meta+a or key meta a for a chord. Use key '+' for a literal plus.",
	"quick":              "Use a semantic action such as app_switch, dismiss_panel, copy, or paste.",
}

// CommandHelp recognizes help only immediately after a command or group name.
// In particular a flag value or later text argument may itself be --help.
func CommandHelp(args []string) (text string, requested bool) {
	if len(args) == 0 {
		return "", false
	}
	var path []string
	switch {
	case args[0] == "help":
		path = args[1:]
	case args[len(args)-1] == "--help" || args[len(args)-1] == "-h":
		path = args[:len(args)-1]
	default:
		return "", false
	}
	if len(path) == 0 {
		return Usage, true
	}
	topic := strings.Join(path, " ")
	var lines []string
	var notes []string
	for _, usage := range commandUsage {
		name := usage
		words := strings.Fields(usage)
		for i, word := range words {
			if strings.HasPrefix(word, "[") || strings.HasPrefix(word, "--") || word == strings.ToUpper(word) {
				name = strings.Join(words[:i], " ")
				break
			}
		}
		if name == topic || strings.HasPrefix(name, topic+" ") {
			lines = append(lines, "  device-cli "+usage)
		}
	}
	if len(lines) == 0 {
		return "", args[0] == "help"
	}
	for i := 1; i <= len(path); i++ {
		if note := helpNotes[strings.Join(path[:i], " ")]; note != "" {
			notes = append(notes, note)
		}
	}
	text = "Usage:\n" + strings.Join(lines, "\n")
	if len(notes) > 0 {
		text += "\n\n" + strings.Join(notes, "\n")
	}
	return text, true
}
