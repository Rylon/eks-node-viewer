/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package model

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/paginator"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/facette/natsort"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/awslabs/eks-node-viewer/pkg/text"
)

var (
	helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262")).Render
	// white / black
	activeDot = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "252"}).Render("•")
	// black / white
	inactiveDot = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "250", Dark: "238"}).Render("•")
)

type UIModel struct {
	progress       progress.Model
	cluster        *Cluster
	extraLabels    []string
	paginator      paginator.Model
	height         int
	nodeSorter     func(lhs, rhs *Node) bool
	style          *Style
	DisablePricing bool
}

func NewUIModel(extraLabels []string, nodeSort string, style *Style) *UIModel {
	pager := paginator.New()
	pager.Type = paginator.Dots
	pager.ActiveDot = activeDot
	pager.InactiveDot = inactiveDot
	return &UIModel{
		// red to green
		progress:    progress.New(style.gradient),
		cluster:     NewCluster(),
		extraLabels: extraLabels,
		paginator:   pager,
		nodeSorter:  makeNodeSorter(nodeSort),
		style:       style,
	}
}

func (u *UIModel) Cluster() *Cluster {
	return u.cluster
}

func (u *UIModel) Init() tea.Cmd {
	return nil
}

func (u *UIModel) View() string {
	b := strings.Builder{}

	stats := u.cluster.Stats()

	sort.Slice(stats.Nodes, func(a, b int) bool {
		return u.nodeSorter(stats.Nodes[a], stats.Nodes[b])
	})

	ctw := text.NewColorTabWriter(&b, 0, 8, 2)
	u.writeClusterSummary(u.cluster.resources, stats, ctw)
	ctw.Flush()
	u.progress.ShowPercentage = true
	// message printer formats numbers nicely with commas
	enPrinter := message.NewPrinter(language.English)
	enPrinter.Fprintf(&b, "%d pods (%d pending %d running %d bound)\n", stats.TotalPods,
		stats.PodsByPhase[v1.PodPending], stats.PodsByPhase[v1.PodRunning], stats.BoundPodCount)

	if stats.NumNodes == 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "Waiting for update or no nodes found...")
		fmt.Fprintln(&b, u.paginator.View())
		fmt.Fprintln(&b, helpStyle("←/→ page • q: quit"))
		return b.String()
	}

	fmt.Fprintln(&b)
	u.paginator.PerPage = u.computeItemsPerPage(stats.Nodes, &b)
	u.paginator.SetTotalPages(stats.NumNodes)
	// check if we're on a page that is outside of the NumNode upper bound
	if u.paginator.Page*u.paginator.PerPage > stats.NumNodes {
		// set the page to the last page
		u.paginator.Page = u.paginator.TotalPages - 1
	}
	start, end := u.paginator.GetSliceBounds(stats.NumNodes)
	if start >= 0 && end >= start {
		// Adds a header row to the table, outside of the paginator.
		fmt.Fprintf(ctw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			"Node name", "", "Resource allocation", "Pods", "Age", "Instance Type", "Compute Type", "Status")

		// Add a column for each of the "extra labels" passed in by the user.
		for _, label := range u.extraLabels {
			// split the label by / and use the last part, to keep column names short
			labelParts := strings.Split(label, "/")
			label = labelParts[len(labelParts)-1]
			label = strings.ToUpper(label[:1]) + label[1:]
			fmt.Fprintf(ctw, "\t%s", label)
		}
		fmt.Fprintln(ctw)

		for _, n := range stats.Nodes[start:end] {
			u.writeNodeInfo(n, ctw, u.cluster.resources)
		}
	}
	ctw.Flush()

	fmt.Fprintln(&b, u.paginator.View())
	fmt.Fprintln(&b, helpStyle("←/→ page • q: quit"))
	return b.String()
}

func (u *UIModel) writeNodeInfo(n *Node, w io.Writer, resources []v1.ResourceName) {
	allocatable := n.Allocatable()
	used := n.Used()
	firstLine := true
	resNameLen := 0
	for _, res := range resources {
		if len(res) > resNameLen {
			resNameLen = len(res)
		}
	}
	for _, res := range resources {
		usedRes := used[res]
		allocatableRes := allocatable[res]
		pct := usedRes.AsApproximateFloat64() / allocatableRes.AsApproximateFloat64()
		if allocatableRes.AsApproximateFloat64() == 0 {
			pct = 0
		}

		if firstLine {

			age := duration.HumanDuration(time.Since(n.Created()))

			priceLabel := fmt.Sprintf("/$%0.4f", n.Price)
			if !n.HasPrice() || u.DisablePricing {
				priceLabel = ""
			}

			maximum_pods := allocatable["pods"]
			pods_text := fmt.Sprintf("%d/%s", n.NumPods(), maximum_pods.String())

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s%s", n.Name(), res, u.progress.ViewAs(pct), pods_text, age, n.InstanceType(), priceLabel)

			// node compute type
			if n.IsOnDemand() {
				fmt.Fprintf(w, "\tOn-Demand")
			} else if n.IsSpot() {
				fmt.Fprintf(w, "\tSpot")
			} else if n.IsFargate() {
				fmt.Fprintf(w, "\tFargate")
			} else {
				fmt.Fprintf(w, "\t-")
			}

			if n.IsAuto() {
				fmt.Fprintf(w, "/Auto")
			}

			readiness, node_status := "", ""

			// node readiness or time we've been waiting for it to be ready
			if n.Ready() {
				// fmt.Fprintf(w, "\tReady")
				readiness = "Ready"
			} else {
				// fmt.Fprintf(w, "\tNotReady/%s", duration.HumanDuration(time.Since(n.NotReadyTime())))
				readiness = fmt.Sprintf("NotReady/%s", duration.HumanDuration(time.Since(n.NotReadyTime())))
			}

			// node status
			if n.Cordoned() && n.Deleting() {
				// fmt.Fprintf(w, "\tCordoned/Deleting")
				node_status = "Cordoned/Deleting"
			} else if n.Deleting() {
				// fmt.Fprintf(w, "\tDeleting")
				node_status = "Deleting"
			} else if n.Cordoned() {
				// fmt.Fprintf(w, "\tCordoned")
				node_status = "Cordoned"
			} //else {
			// fmt.Fprintf(w, "\t-")
			// }

			if node_status != "" {
				node_status = fmt.Sprintf("(%s)", node_status)
			}

			// Combine node status and readiness into a single column
			// fmt.Fprintf(w, "\t%s/%s", n.Status(), n.Ready())
			fmt.Fprintf(w, "\t%s%s", readiness, node_status)

			for _, label := range u.extraLabels {
				labelValue, ok := n.node.Labels[label]
				if !ok {
					// support computed label values
					labelValue = n.ComputeLabel(label)
				}
				fmt.Fprintf(w, "\t%s", labelValue)
			}

		} else {
			fmt.Fprintf(w, " \t%s\t%s\t\t\t\t\t\t", res, u.progress.ViewAs(pct))
			for range u.extraLabels {
				fmt.Fprintf(w, "\t")
			}
		}
		fmt.Fprintln(w)
		firstLine = false
	}
}

func (u *UIModel) writeClusterSummary(resources []v1.ResourceName, stats Stats, w io.Writer) {
	firstLine := true

	for _, res := range resources {
		allocatable := stats.AllocatableResources[res]
		used := stats.UsedResources[res]

		usedStr := used.String()
		allocatableStr := allocatable.String()

		// message printer formats numbers nicely with commas
		enPrinter := message.NewPrinter(language.English)

		// Normalise memory to Mi for consistent display
		if res == v1.ResourceMemory {
			var usedMi, allocatableMi float64

			if used.Value() != 0 {
				usedMi = float64(used.Value()) / (1024 * 1024) // Convert to Mi
			}

			if allocatable.Value() != 0 {
				allocatableMi = float64(allocatable.Value()) / (1024 * 1024) // Convert to Mi
			}

			usedStr = enPrinter.Sprint(int64(usedMi)) + "Mi"
			allocatableStr = enPrinter.Sprint(int64(allocatableMi)) + "Mi"
		}

		pctUsed := 0.0
		if allocatable.AsApproximateFloat64() != 0 {
			pctUsed = 100 * (used.AsApproximateFloat64() / allocatable.AsApproximateFloat64())
		}
		pctUsedStr := fmt.Sprintf("%0.1f%%", pctUsed)
		if pctUsed > 90 {
			pctUsedStr = u.style.green(pctUsedStr)
		} else if pctUsed > 60 {
			pctUsedStr = u.style.yellow(pctUsedStr)
		} else {
			pctUsedStr = u.style.red(pctUsedStr)
		}

		u.progress.ShowPercentage = false
		monthlyPrice := stats.TotalPrice * (365 * 24) / 12 // average hours per month

		clusterPrice := enPrinter.Sprintf("$%0.3f/hour | $%0.3f/month", stats.TotalPrice, monthlyPrice)
		if u.DisablePricing {
			clusterPrice = ""
		}
		if firstLine {
			enPrinter.Fprintf(w, "%d nodes\t(%10s/%s)\t%s\t%s\t%s\t%s\n",
				stats.NumNodes, usedStr, allocatableStr, pctUsedStr, res, u.progress.ViewAs(pctUsed/100.0), clusterPrice)
		} else {
			enPrinter.Fprintf(w, " \t%s/%s\t%s\t%s\t%s\t\n",
				usedStr, allocatableStr, pctUsedStr, res, u.progress.ViewAs(pctUsed/100.0))
		}
		firstLine = false
	}
}

// computeItemsPerPage dynamically calculates the number of lines we can fit per page
// taking into account header and footer text
func (u *UIModel) computeItemsPerPage(nodes []*Node, b *strings.Builder) int {
	var buf bytes.Buffer
	u.writeNodeInfo(nodes[0], &buf, u.cluster.resources)
	headerLines := strings.Count(b.String(), "\n") + 2
	nodeLines := strings.Count(buf.String(), "\n")
	if nodeLines == 0 {
		nodeLines = 1
	}
	return ((u.height - headerLines) / nodeLines) - 1
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (u *UIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		u.height = msg.Height
		return u, tickCmd()
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return u, tea.Quit
		}
	case tickMsg:
		return u, tickCmd()
	}
	var cmd tea.Cmd
	u.paginator, cmd = u.paginator.Update(msg)
	return u, cmd
}

func (u *UIModel) SetResources(resources []string) {
	u.cluster.resources = nil
	for _, r := range resources {
		u.cluster.resources = append(u.cluster.resources, v1.ResourceName(r))
	}
}

func makeNodeSorter(nodeSort string) func(lhs *Node, rhs *Node) bool {
	sortOrder := func(b bool) bool { return b }
	if strings.HasSuffix(nodeSort, "=asc") {
		nodeSort = nodeSort[:len(nodeSort)-4]
	}
	if strings.HasSuffix(nodeSort, "=dsc") {
		sortOrder = func(b bool) bool { return !b }
		nodeSort = nodeSort[:len(nodeSort)-4]
	}

	if nodeSort == "creation" {
		return func(lhs *Node, rhs *Node) bool {
			if lhs.Created() == rhs.Created() {
				return sortOrder(natsort.Compare(lhs.Name(), rhs.Name()))
			}
			return sortOrder(rhs.Created().Before(lhs.Created()))
		}
	}

	return func(lhs *Node, rhs *Node) bool {
		lhsLabel, ok := lhs.node.Labels[nodeSort]
		if !ok {
			lhsLabel = lhs.ComputeLabel(nodeSort)
		}
		rhsLabel, ok := rhs.node.Labels[nodeSort]
		if !ok {
			rhsLabel = rhs.ComputeLabel(nodeSort)
		}
		if lhsLabel == rhsLabel {
			return sortOrder(natsort.Compare(lhs.InstanceID(), rhs.InstanceID()))
		}
		return sortOrder(natsort.Compare(lhsLabel, rhsLabel))
	}
}
