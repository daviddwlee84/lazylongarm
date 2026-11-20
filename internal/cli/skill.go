package cli

import (
	"fmt"
	"io"

	"github.com/daviddwlee84/lazylongarm/internal/skill"
	"github.com/spf13/cobra"
)

func printSkill(w io.Writer, topic string) error {
	text, err := skill.Read(topic)
	if err != nil {
		return usage("%v", err)
	}
	_, err = fmt.Fprint(w, text)
	return err
}

func skillCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "skill", Short: "Print the operating guide or install agent skills", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(skillSyncCommand(o))
	c.AddCommand(&cobra.Command{
		Use: "print [topic]", Short: "Print SKILL.md or a reference topic", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			topic := ""
			if len(args) == 1 {
				topic = args[0]
			}
			return printSkill(cmd.OutOrStdout(), topic)
		},
	})
	return c
}
