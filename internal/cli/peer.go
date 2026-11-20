package cli

import "github.com/spf13/cobra"

func peerCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "peer", Short: "Bootstrap Longarm peers: workers whose own longarm can dispatch too", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(peerAddCommand(o))
	return c
}

func peerAddCommand(o *options) *cobra.Command {
	return machineAddCommand(o, &workerAddFlags{peer: true}, &cobra.Command{
		Use:   "add [ssh-host]",
		Short: "Make a machine a Longarm worker AND enroll its longarm as a client (wizard)",
		Long: `Everything ` + "`lazylongarm worker add`" + ` does, then enroll the machine's own longarm as a
client of the same hub (a one-time client code passed over ssh stdin), so a
shell or agent session on it (herdr, ssh) can run longarm against the other
workers. On a machine that is already a worker only the client steps run.

The client is a separate node, named <worker>-client by default (node names are
unique on the hub), and needs an admin certificate to mint. Jobs on the machine
run as the same account that holds the client certificate, so any job there can
dispatch to every worker you can use: the review says so, and louder when the
account is root-equivalent (docker/lxd/libvirt/disk group, passwordless sudo).
Take it back with ` + "`longarm admin revoke <worker>-client`" + `.`,
		Example: `  lazylongarm peer add
  lazylongarm peer add build-box --dry-run
  lazylongarm peer add build-box --client-name build-box-cli`,
	})
}
