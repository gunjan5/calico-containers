// Copyright (c) 2016 Tigera, Inc. All rights reserved.

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

package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	"github.com/docopt/docopt-go"
	gops "github.com/mitchellh/go-ps"
	"github.com/olekukonko/tablewriter"
	"github.com/projectcalico/calico-containers/calicoctl/commands/argutils"
)

// Check for Word_<IP> where every octate is seperated by "_", regardless of IP protocols
// Example match: "Mesh_192_168_56_101" or "Mesh_fd80_24e2_f998_72d7__2"
var bgpPeerRegex, _ = regexp.Compile(`[A-Za-z]+\_\w+\b`)

// Status prings status of the node and returns error (if any)
func Status(args []string) {
	doc := `Usage:
  calicoctl node status

Options:
  -h --help                 Show this screen.
     --backend=(bird|gobgp|none)
                           Specify which networking backend to use.  When set
                           to "none", Calico node runs in policy only mode.
                           The option to run with gobgp is currently
                           experimental.
                           [default: bird]

Description:
  Check the status of the Calico node instance.  This incudes the status and
  uptime of the node instance, and BGP peering states.
`

	parsedArgs, err := docopt.Parse(doc, args, true, "", false, false)
	if err != nil {
		fmt.Printf("Invalid option: 'calicoctl %s'. Use flag '--help' to read about a specific subcommand.\n", strings.Join(args, " "))
		os.Exit(1)
	}
	if len(parsedArgs) == 0 {
		return
	}

	// Must run this command as root to be able to connect to BIRD sockets
	enforceRoot()

	backend := argutils.ArgStringOrBlank(parsedArgs, "--backend")

	backendMatch := regexp.MustCompile("^(none|bird|gobgp)$")
	if !backendMatch.MatchString(backend) {
		fmt.Printf("Error executing command: unknown backend '%s'\n", backend)
		os.Exit(1)
	}

	// Go through running processes and check if `calico-felix` processes is not running
	processes, err := gops.Processes()
	if err != nil {
		fmt.Println(err)
	}
	if !psContains("calico-felix", processes) {
		// Return and print message if calico-node is not running
		fmt.Printf("Calico process is not running.\n")
		os.Exit(1)
	}

	fmt.Printf("Calico process is running.\n")
	switch backend {
	case "bird":
		// Check if birdv4 process is running, print the BGP peer table if it is, else print a warning
		if psContains("bird", processes) {
			printBGPPeers("4")
		} else {
			fmt.Printf("\nINFO: BIRDv4 process: 'bird' is not running.\n")
		}

		// Check if birdv6 process is running, print the BGP peer table if it is, else print a warning
		if psContains("bird6", processes) {
			printBGPPeers("6")
		} else {
			fmt.Printf("\nINFO: BIRDv6 process: 'bird6' is not running.\n")
		}

	case "gobgp":
		// Check if GoBGP process is running, print the BGP peer table if it is, else print a warning.
		if psContains("calico-bgp-daemon", processes) {
			printBGPPeers("4")
		} else {
			fmt.Printf("\nINFO: BIRDv4 process: 'bird' is not running.\n")
		}

	}

	// Have to manually enter an empty line because the table print
	// library prints the last line, so can't insert a '\n' there
	fmt.Println()
}

func psContains(proc string, procList []gops.Process) bool {
	for _, p := range procList {
		if p.Executable() == proc {
			return true
		}
	}
	return false
}

func printBGPPeers(ipv string) {
	birdSuffix := ""
	ipSep := "."
	if ipv == "6" {
		birdSuffix = "6"
		ipSep = ":"
	}

	fmt.Printf("\nIPv%s BGP status", ipv)

	// Try connecting to the bird socket in `/var/run/calico/` first to get the data
	c, err := net.Dial("unix", fmt.Sprintf("/var/run/calico/bird%s.ctl", birdSuffix))
	if err != nil {

		// If that fails, try connecting to bird socket in `/var/run/bird` (which is the
		// default socket location for bird install) for non-containerized installs
		c, err = net.Dial("unix", fmt.Sprintf("/var/run/bird/bird%s.ctl", birdSuffix))
		if err != nil {
			fmt.Printf("Error connecting to BIRDv%s socket: %v", ipv, err)
			return
		}
	}
	defer c.Close()

	fmt.Println()

	_, err = c.Write([]byte("show protocols\n"))
	if err != nil {
		fmt.Printf("Error writing to BIRD socket: %s\n", err)
		os.Exit(1)
	}

	buf := make([]byte, 1024)

	n, err := c.Read(buf[:])
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	data := [][]string{}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Peer address", "Peer type", "State", "Since", "Info"})

	birdOut := string(buf[:n])

	for _, line := range strings.Split(birdOut, "\n") {

		ipString := bgpPeerRegex.FindString(line)

		if ipString != "" {
			col := []string{}

			// `f` is a temp []string to hold all the words starting from 3rd to end
			// Ideally the `line` should be something like "Mesh_172_17_8_102 BGP master up 22:23:45 Established",
			// but in case of "Mesh_fd80_24e2_f998_72d7__2 BGP  master   start  17:56:21 Active Socket: Connection closed"
			// providing only "Active" in the Info section is not enough, so we append rest of the info into the last field
			f := strings.Fields(line)[3:]
			fields := make([]string, 3)
			copy(fields, f[0:3])

			if len(f) > 3 {
				// We are appending all the extra fields to the last element in the slice.
				// This is to include the extra info when the "Info" field is other than "Established"
				for _, e := range f[3:] {
					fields[2] = fields[2] + " " + e
				}
			}

			if strings.HasPrefix(ipString, "Mesh_") {
				ipString = ipString[5:]
				ipString = strings.Replace(ipString, "_", ipSep, -1)
				col = append(col, ipString)
				col = append(col, "node-to-node mesh")
				col = append(col, fields...)
			} else if strings.HasPrefix(ipString, "Node_") {
				ipString = ipString[5:]
				ipString = strings.Replace(ipString, "_", ipSep, -1)
				col = append(col, ipString)
				col = append(col, "node specific")
				col = append(col, fields...)
			} else if strings.HasPrefix(ipString, "Global_") {
				ipString = ipString[7:]
				ipString = strings.Replace(ipString, "_", ipSep, -1)
				col = append(col, ipString)
				col = append(col, "global")
				col = append(col, fields...)
			} else {
				// Did not match any of the pre-defined options for BIRD
				fmt.Println(errors.New("Error: Did not match any of the predefined options for BIRD"))
				break
			}
			data = append(data, col)
		}
	}

	for _, v := range data {
		table.Append(v)
	}

	if len(data) == 0 {
		fmt.Printf("No IPv%s peers found.\n", ipv)
		return
	}

	table.Render()
}
