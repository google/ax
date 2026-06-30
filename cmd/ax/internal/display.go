// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	"fmt"
	"io"
	"os"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/google/ax/proto"
)

const (
	boxWidth = 100
)

var (
	isDark    = lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
	lightDark = lipgloss.LightDark(isDark)

	purple  = lightDark(lipgloss.Color("#5A56E0"), lipgloss.Color("#7571F9"))
	comment = lightDark(lipgloss.Color("#a0a0a0"), lipgloss.Color("#6d6d6d"))
)

// ErrUserAborted is returned when the user aborts a prompt.
var ErrUserAborted = huh.ErrUserAborted

type displayState int

const (
	stateNone displayState = iota
	stateText
	stateThought
)

type Display struct {
	id string
	w  io.Writer // Target output writer, e.g., os.Stdout or a test buffer

	userStyle       lipgloss.Style
	checkpointStyle lipgloss.Style
	idStyle         lipgloss.Style
	resumeStyle     lipgloss.Style

	state displayState // Tracks the last printed chunk type to correctly format transition newlines
}

func NewDisplay(id string, w io.Writer) *Display {
	if w == nil {
		w = os.Stdout
	}
	return &Display{
		id:              id,
		w:               w,
		userStyle:       lipgloss.NewStyle().Foreground(purple),
		checkpointStyle: lipgloss.NewStyle().Foreground(comment),
		idStyle:         lipgloss.NewStyle().Foreground(comment),
		resumeStyle:     lipgloss.NewStyle().Foreground(comment),
		state:           stateNone,
	}
}

// DisplayInput displays the user input.
func (d *Display) DisplayInput(text string) {
	if d.state != stateNone {
		fmt.Fprintln(d.w)
	}
	d.state = stateNone
	fmt.Fprintf(d.w, "%s %s\n",
		d.userStyle.Render("⏺"),
		text,
	)
	fmt.Fprintln(d.w)
}

// Display prints a content block according to its type.
func (d *Display) Display(content *proto.Content) {
	if content == nil {
		return
	}
	switch o := content.Type.(type) {
	case *proto.Content_Text:
		if d.state == stateThought {
			fmt.Fprintln(d.w) // end the thinking line
		}
		d.state = stateText
		fmt.Fprint(d.w, o.Text.Text)

	case *proto.Content_Confirmation:
		// Let the confirmation prompt handle displaying the question.

	case *proto.Content_ToolCall:
		// No-op for cleaner CLI logs

	case *proto.Content_ToolResult:
		// Only print if the tool returned an error, otherwise skip
		tr := o.ToolResult
		if fr := tr.GetFunctionResult(); fr != nil {
			if fr.GetResponse() != nil {
				respMap := fr.GetResponse().AsMap()
				if errStr, ok := respMap["error"]; ok {
					d.displaySystem(fmt.Sprintf("[TOOL ERROR for %s]\n%v", fr.Name, errStr))
				}
			}
		}

	case *proto.Content_Thought:
		for _, summary := range o.Thought.GetSummary() {
			if textContent := summary.GetText(); textContent != nil {
				if d.state != stateThought {
					if d.state == stateText {
						fmt.Fprintln(d.w)
					}
					fmt.Fprint(d.w, "Thinking: ")
				}
				d.state = stateThought
				fmt.Fprint(d.w, textContent.Text)
			}
		}

	case *proto.Content_Image, *proto.Content_Audio, *proto.Content_Video, *proto.Content_Document:
		d.displaySystem(fmt.Sprintf("unsupported output type for display: %T", o))

	default:
		d.displaySystem(fmt.Sprintf("unknown output type: %v", o))
	}
}

// displaySystem prints a system/error message on a new line.
func (d *Display) displaySystem(text string) {
	if d.state != stateNone {
		fmt.Fprintln(d.w)
	}
	d.state = stateNone
	fmt.Fprintln(d.w, text)
}

// ShowNotice prints an informational message (e.g. the current harness config
// or a validation error) on its own line.
func (d *Display) ShowNotice(text string) {
	d.displaySystem(text)
}

// FinishOutput completes the streaming output and shows info if provided
func (d *Display) FinishOutput(info string) {
	if d.state != stateNone {
		fmt.Fprintln(d.w)
	}
	d.state = stateNone
	if info != "" {
		fmt.Fprintln(d.w, d.checkpointStyle.Render(info))
	}
	fmt.Fprintln(d.w)
}

func (d *Display) DisplayHeader() {
	fmt.Fprintln(d.w, d.idStyle.Render("Conversation: "+d.id))
	fmt.Fprintln(d.w)
}

// PromptForApproval shows an accept/reject dialog
// Returns true if accepted, false if rejected, and an error if cancelled (Ctrl+C)
func (d *Display) PromptForApproval(question string) (bool, error) {
	var accepted bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(question).
				Affirmative("Accept").
				Negative("Reject").
				Value(&accepted),
		),
	).WithWidth(boxWidth)

	if err := form.Run(); err != nil {
		return false, err
	}
	return accepted, nil
}

// PromptForInput shows the input box and returns the user input
// Returns the input string and an error if the user cancelled (Ctrl+C)
func (d *Display) PromptForInput() (string, error) {
	var userInput string

	// Rebind tab to complete the suggestion and enter to submit.
	keymap := huh.NewDefaultKeyMap()
	keymap.Input.AcceptSuggestion = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "complete"))
	keymap.Input.Next = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit"))

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Placeholder("Enter prompt... (type `q` to quit)").
				Suggestions([]string{"/config"}).
				Value(&userInput),
		),
	).WithWidth(boxWidth).WithShowHelp(false).WithKeyMap(keymap)

	if err := form.Run(); err != nil {
		return "", err
	}
	return userInput, nil
}

// configKeyMap returns a huh keymap for the /config menu that cancels on esc in
// addition to the default ctrl+c, and shows a consistent "esc close" hint in
// every field's help footer.
func configKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "close"))

	// Surface the esc hint through a slot that stays enabled.
	closeHint := key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close"))
	km.Select.Filter = closeHint
	km.Text.Editor = closeHint
	return km
}

// clearMenuOnCancel converts the interrupt huh emits on cancel (esc/ctrl+c) into
// a graceful quit.
var clearMenuOnCancel = tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.InterruptMsg); ok {
		return tea.QuitMsg{}
	}
	return msg
})

// PromptForConfigAction shows the /config menu and returns the chosen action,
// one of "edit", "load", or "cancel". It returns an error if the user cancelled
// (Ctrl+C or Esc).
func (d *Display) PromptForConfigAction() (string, error) {
	var action string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Harness config").
				Options(
					huh.NewOption("View/edit config", "edit"),
					huh.NewOption("Load from file", "load"),
					huh.NewOption("Cancel", "cancel"),
				).
				Value(&action),
		),
	).WithWidth(boxWidth).WithKeyMap(configKeyMap()).WithProgramOptions(clearMenuOnCancel)

	if err := form.Run(); err != nil {
		return "", err
	}
	return action, nil
}

// PromptForConfigEdit shows a multi-line editor pre-filled with current and
// returns the edited text. It returns an error if the user cancelled (Ctrl+C or
// Esc).
func (d *Display) PromptForConfigEdit(current string) (string, error) {
	text := current
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewText().
				Title("Harness config (JSON, leave empty to clear)").
				Lines(10).
				ExternalEditor(true).
				Value(&text),
		),
	).WithWidth(boxWidth).WithKeyMap(configKeyMap()).WithProgramOptions(clearMenuOnCancel)

	if err := form.Run(); err != nil {
		return "", err
	}
	return text, nil
}

// PromptForConfigFile shows a file browser for selecting a JSON config file and
// returns the chosen path. It returns an error if the user cancelled (Ctrl+C or
// Esc).
func (d *Display) PromptForConfigFile() (string, error) {
	var path string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewFilePicker().
				Title("Load harness config").
				CurrentDirectory(".").
				AllowedTypes([]string{".json"}).
				FileAllowed(true).
				DirAllowed(false).
				Picking(true).
				Height(10).
				Value(&path),
		),
	).WithWidth(boxWidth).WithKeyMap(configKeyMap()).WithProgramOptions(clearMenuOnCancel)

	if err := form.Run(); err != nil {
		return "", err
	}
	return path, nil
}

func (d *Display) ShowResumption(id string, server string) {
	fmt.Fprintln(d.w, d.resumeStyle.Render("To resume the conversation,"))
	if server != "" {
		fmt.Fprintln(d.w, d.resumeStyle.Render(fmt.Sprintf("ax exec --conversation %s --server %s", id, server)))
	} else {
		fmt.Fprintln(d.w, d.resumeStyle.Render(fmt.Sprintf("ax exec --conversation %s", id)))
	}
}
