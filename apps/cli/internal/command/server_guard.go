package command

import (
	"fmt"
	"io"
)

// writeServerTokenMismatch explains a saved-login vs --server conflict.
func writeServerTokenMismatch(writer io.Writer, savedServer, targetServer string) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s\n", style.boldYellow("Login does not match --server"))
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Saved"), style.value(displayServer(savedServer)))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Target"), style.value(displayServer(targetServer)))
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s\n", style.dim("Pick one:"))
	fmt.Fprintf(writer, "  %s  %s\n", style.dim("·"), "doppels logout          # then retry (anonymous ok for share)")
	fmt.Fprintf(writer, "  %s  %s\n", style.dim("·"), "doppels login --server "+displayServer(targetServer))
	fmt.Fprintf(writer, "  %s  %s\n", style.dim("·"), "DOPPELS_API_TOKEN=… for this target only")
	fmt.Fprintln(writer)
}

func displayServer(server string) string {
	if server == "" {
		return "(unknown)"
	}
	return server
}
