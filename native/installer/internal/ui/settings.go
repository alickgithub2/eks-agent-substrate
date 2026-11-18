package ui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// settingsScreen edits what config.env controls for the run: the new
// cluster's knobs (only when the run creates the cluster), the sandbox
// classes, the node groups they make (only those), TAGS, the release and
// its image repository, and the database. Values are checked with
// state.ValidateConfig; n applies them to the Setup and moves on, and the
// install writes them to config.local.env when it starts (Review).
type settingsScreen struct {
	fields  []field
	cursor  int
	values  map[string]string // config keys, plus dbKey
	errs    map[string]string
	editing bool
	input   string
}

type fieldKind int

const (
	fText fieldKind = iota
	fToggle
	fClass    // one sandbox class, on or off
	fDatabase // RDS or the bundled PostgreSQL
)

// The values that are not edited as text: the classes (config.env's
// SANDBOX_CLASSES, toggled one class at a time) and the database.
const (
	classKey = "SANDBOX_CLASSES"
	dbKey    = "database"
)

// field is one focusable setting. Node group rows pair an instance type
// with a node count (group set).
type field struct {
	kind    fieldKind
	key     string // config key, or the sandbox class
	label   string
	section string
	group   string
}

// settingsFields are the fields for a new or connected cluster, with node
// group rows for groups, the node groups a new cluster gets.
func settingsFields(newCluster bool, groups []string) []field {
	var f []field
	if newCluster {
		f = append(f,
			field{kind: fText, key: "KUBERNETES_VERSION", label: "Kubernetes version", section: "New cluster"},
			field{kind: fToggle, key: "AUTO_MODE", label: "EKS Auto Mode", section: "New cluster"},
			field{kind: fText, key: "EKS_PUBLIC_ACCESS_CIDRS", label: "API access CIDRs", section: "New cluster"})
	}
	for _, c := range state.Classes {
		f = append(f, field{kind: fClass, key: c, label: state.ClassTitle(c), section: "Sandbox classes"})
	}
	if newCluster {
		for _, g := range groups {
			up := strings.ToUpper(g)
			f = append(f,
				field{kind: fText, key: up + "_INSTANCE_TYPE", label: "instance type", section: "Node groups", group: g},
				field{kind: fText, key: up + "_NODES", label: "nodes", section: "Node groups", group: g})
		}
	}
	return append(f,
		field{kind: fText, key: "TAGS", label: "Tags", section: "Every AWS resource"},
		field{kind: fText, key: "SUBSTRATE_TAG", label: "Release", section: "Agent Substrate"},
		field{kind: fText, key: "SUBSTRATE_IMAGE_REPO", label: "Image repository", section: "Agent Substrate"},
		field{kind: fDatabase, key: dbKey, label: "Database", section: "Agent Substrate"})
}

// classesHelp is what every class's help ends with: the classes decide the
// node groups.
const classesHelp = " The classes decide a new cluster's node groups: gVisor and microVM make theirs. " +
	"The system group comes without Auto Mode, with either of them and RDS (the control plane is pinned there), " +
	"and with a plugin that runs on it. With neither class, an Auto Mode cluster gets a node group only from a plugin: " +
	"mode 3 adds the microvm group (and with RDS the system group), and a plugin that runs on the system group " +
	"(the LLM gateway, EFS, or login rotation with RDS) adds that one. Without such a plugin it gets none: " +
	"the control plane, the bundled PostgreSQL and every controller run on Auto Mode nodes. Press m for that minimal profile."

// help is a setting's explanation when config.env has no comment above it.
var help = map[string]string{
	"KUBERNETES_VERSION":      "The EKS version of the new cluster. Substrate needs 1.37 or newer (the PodCertificate APIs).",
	"AUTO_MODE":               "EKS Auto Mode, with its general-purpose and system NodePools next to the node groups. Fixed when the cluster is created. The AWS Nitro sandbox class needs it.",
	"EKS_PUBLIC_ACCESS_CIDRS": "Address ranges allowed to reach the Kubernetes API endpoint, space-separated. Requests are still authenticated by IAM.",
	"TAGS":                    "Space-separated key=value tags on every AWS resource the install creates. The owner tag agent-substrate-on-eks/deployment=<cluster>.<region> is always added.",
	"SUBSTRATE_TAG":           "The Agent Substrate release to install, such as v1.0.0. An install runs one release: with the AWS Nitro sandbox class (or the mode 3 plugin) it is the fork's release that class installs, and this one is kept for a run without it.",
	"SUBSTRATE_IMAGE_REPO":    "Where the release's images come from.",
	state.ClassGVisor:         "gVisor runs actors in a user-space kernel, on the gvisor node group." + classesHelp,
	state.ClassMicroVM:        "microVM runs actors in Kata microVMs on the microvm node group, whose instance type needs nested virtualization." + classesHelp,
	state.ClassNitro: "The AWS Nitro sandbox makes each worker Pod an EKS Auto Mode isolated sandbox (RuntimeClass isolated-sandbox) " +
		"on nodes Auto Mode launches for it, so it needs Auto Mode and makes no node group. It installs the fork's release " +
		"(one release per install), and is set up and checked in its own step after the control plane." + classesHelp,
	dbKey: "Where the control plane keeps its state: Amazon RDS for PostgreSQL (adds the Create the database step), or the bundled PostgreSQL in the cluster. " +
		"On a new Auto Mode cluster with a gVisor or microVM group, RDS pins the control plane to the system node group, which is made for it; " +
		"with the bundled PostgreSQL the control plane runs on Auto Mode nodes, and the system group is made only for a plugin that runs on it.",
	"SYSTEM_INSTANCE_TYPE":  "The system node group runs the controllers pinned to it: the control plane with RDS on Auto Mode, and the plugins' controllers.",
	"GVISOR_INSTANCE_TYPE":  "The gvisor node group runs gVisor actors.",
	"MICROVM_INSTANCE_TYPE": "The microvm node group runs microVM actors; its instance type needs nested virtualization (m8i, c8i, r8i).",
}

// noGroupsLine is the Node groups section of a new cluster that gets none.
const noGroupsLine = "none: everything runs on EKS Auto Mode nodes"

// classNote is a class's note beside its box: where it runs.
var classNote = map[string]string{
	state.ClassGVisor:  "gvisor node group",
	state.ClassMicroVM: "microvm node group",
	state.ClassNitro:   "on Auto Mode nodes",
}

// classShort is the note of a class this run cannot have; the panel beside
// says why in full.
func (m *Model) classShort(c string) string {
	if c == state.ClassNitro {
		return "needs EKS Auto Mode"
	}
	return "no " + c + " node group"
}

func (m *Model) enterSettings() {
	st := m.set
	st.values = state.ConfigValues(m.s)
	st.values[dbKey] = m.s.Database
	st.errs = map[string]string{}
	st.editing = false
	m.refreshFields()
	st.cursor = min(st.cursor, len(st.fields)-1)
	m.checkSettings()
}

// autoMode is the run's Auto Mode as Settings has it now: the AUTO_MODE
// value for a new cluster, the cluster's own otherwise.
func (m *Model) settingsAutoMode() bool {
	if m.s.ClusterIsNew {
		return m.set.values["AUTO_MODE"] == "on"
	}
	return m.s.AutoMode
}

// settingsGroups are the node groups of the cluster as Settings has it now.
func (m *Model) settingsGroups() []string {
	if m.s.ClusterIsNew {
		return state.NewNodeGroups(m.settingsAutoMode(), m.set.values[dbKey], m.set.values[classKey], m.s.Plugins)
	}
	return m.s.NodeGroups()
}

// refreshFields rebuilds the fields for the current values, whose classes
// and Auto Mode decide the node group rows, keeping the cursor on its key.
func (m *Model) refreshFields() {
	st := m.set
	at := ""
	if st.cursor >= 0 && st.cursor < len(st.fields) {
		at = st.fields[st.cursor].key
	}
	st.fields = settingsFields(m.s.ClusterIsNew, m.settingsGroups())
	for i, f := range st.fields {
		if f.key == at {
			st.cursor = i
		}
	}
	st.cursor = min(st.cursor, len(st.fields)-1)
}

func (st *settingsScreen) classOn(c string) bool { return state.HasClass(st.values[classKey], c) }

// classProblem is why class c cannot be on for this run, or "".
func (m *Model) classProblem(c string) string {
	return state.ClassProblem(c, m.settingsAutoMode(), m.s.ClusterIsNew, m.s.ClusterName, m.s.ClusterPools)
}

// toggleClass turns a sandbox class on or off, keeping at least one, in
// state.Classes' order; a class this run cannot have is not turned on.
func (m *Model) toggleClass(c string) string {
	st := m.set
	if !st.classOn(c) {
		if why := m.classProblem(c); why != "" {
			return state.ClassTitle(c) + " " + why
		}
	}
	on := state.FormatClasses(func(k string) bool { return st.classOn(k) != (k == c) })
	if on == "" {
		return "At least one sandbox class is installed."
	}
	st.values[classKey] = on
	return ""
}

// minimal is the m key: the minimal profile, Auto Mode with the AWS Nitro
// sandbox class only and the bundled PostgreSQL. A new cluster then gets no
// node group, and everything runs on Auto Mode nodes: a chosen plugin that
// would add one is cleared, and the notice names it.
func (m *Model) minimal() string {
	st := m.set
	if m.s.ClusterIsNew {
		st.values["AUTO_MODE"] = "on"
	} else if !m.s.AutoMode {
		return "The minimal profile needs EKS Auto Mode, and " + m.s.ClusterName + " has it off."
	}
	st.values[classKey], st.values[dbKey] = state.ClassNitro, state.DatabaseBundled
	if !m.s.ClusterIsNew {
		return "Minimal profile: the AWS Nitro sandbox only and the bundled PostgreSQL."
	}
	if cleared := m.clearGroupPlugins(); len(cleared) > 0 {
		return "Cleared " + andList(cleared) + " for the minimal profile (no node group); choose again on the Plugins screen to keep the group"
	}
	return "Minimal profile: EKS Auto Mode, the AWS Nitro sandbox only, the bundled PostgreSQL; no node group."
}

// clearGroupPlugins clears every chosen plugin that would give the minimal
// profile's new cluster a node group (mode 3's microvm group, the system
// group of a plugin that runs on it), so the profile keeps its promise, and
// returns their IDs. Plugins that add no group stay chosen. A selection the
// Plugins screen has not made yet is only the defaults, which it then
// preselects for the minimal profile's settings (enterPlugins).
func (m *Model) clearGroupPlugins() []string {
	ids := m.s.Plugins
	if m.pl.shown {
		ids = m.chosenIDs()
	}
	var cleared []string
	for _, id := range ids {
		if len(state.NewNodeGroups(true, state.DatabaseBundled, state.ClassNitro, []string{id})) > 0 {
			cleared = append(cleared, id)
		}
	}
	if len(cleared) == 0 {
		return nil
	}
	if m.pl.shown {
		for _, id := range cleared {
			m.setPlugin(id, false)
		}
		m.s.Plugins = m.chosenIDs()
	} else {
		m.s.Plugins = slices.DeleteFunc(slices.Clone(m.s.Plugins), func(id string) bool { return slices.Contains(cleared, id) })
	}
	return cleared
}

// checkSettings validates every value, for the screen and for n: the config
// keys (only the node groups shown, the ones the cluster gets), each class
// against the cluster, and the node group rule.
func (m *Model) checkSettings() {
	st := m.set
	st.errs = state.ValidateConfig(st.values)
	shown := map[string]bool{}
	for _, f := range st.fields {
		shown[f.key] = true
	}
	for _, g := range nodegroups.All {
		for _, k := range []string{strings.ToUpper(g) + "_INSTANCE_TYPE", strings.ToUpper(g) + "_NODES"} {
			if !shown[k] {
				delete(st.errs, k)
			}
		}
	}
	if _, bad := st.errs[classKey]; bad {
		return
	}
	// Short, to fit under the classes; the class's panel says why in full.
	for _, c := range state.ParseClasses(st.values[classKey]) {
		if m.classProblem(c) != "" {
			st.errs[classKey] = state.ClassTitle(c) + ": " + m.classShort(c)
			return
		}
	}
	// Defensive: cannot fire today, as classProblem above refuses the only set Check refuses (nitro alone, Auto Mode off).
	if m.s.ClusterIsNew {
		if err := nodegroups.Check(m.settingsAutoMode(), m.settingsGroups()); err != nil {
			st.errs[classKey] = err.Error()
		}
	}
}

// release is the release the run installs with the current classes, and
// what sets it when a class or plugin replaces SUBSTRATE_TAG's ("" when
// none does).
func (m *Model) settingsRelease() (plugin.Release, string) {
	st := m.set
	ids := state.WithClassPlugins(m.s.Plugins, st.values[classKey])
	rel := plugin.ChosenRelease(ids, st.values["SUBSTRATE_TAG"], st.values["SUBSTRATE_IMAGE_REPO"])
	if rel.Repo == plugin.UpstreamRepo {
		return rel, ""
	}
	if st.classOn(state.ClassNitro) {
		return rel, "the AWS Nitro sandbox class"
	}
	return rel, "a chosen plugin"
}

// apply writes the answers to the Setup. A new cluster is recorded again,
// so its version and Auto Mode follow the edited knobs (review F13).
func (m *Model) applySettings() {
	v := m.set.values
	c := &m.s.NewClusterConfig
	for _, k := range []struct {
		key string
		dst *string
	}{
		{"KUBERNETES_VERSION", &c.KubernetesVersion}, {"AUTO_MODE", &c.AutoMode}, {"EKS_PUBLIC_ACCESS_CIDRS", &c.PublicAccessCIDRs},
		{"SYSTEM_INSTANCE_TYPE", &c.SystemInstanceType}, {"GVISOR_INSTANCE_TYPE", &c.GVisorInstanceType}, {"MICROVM_INSTANCE_TYPE", &c.MicroVMInstanceType},
		{"SYSTEM_NODES", &c.SystemNodes}, {"GVISOR_NODES", &c.GVisorNodes}, {"MICROVM_NODES", &c.MicroVMNodes},
		{"TAGS", &m.s.Tags}, {"SUBSTRATE_TAG", &m.s.SubstrateTag}, {"SUBSTRATE_IMAGE_REPO", &m.s.ImageRepo},
		{classKey, &m.s.SandboxClasses}, {dbKey, &m.s.Database},
	} {
		*k.dst = v[k.key]
	}
	if m.s.ClusterIsNew {
		m.s.NewCluster(m.s.ClusterName)
	}
}

func (m *Model) settingsKey(msg tea.KeyMsg) tea.Cmd {
	st := m.set
	f := st.fields[st.cursor]
	key := msg.String()
	if st.editing {
		switch key {
		case "enter":
			st.values[f.key] = strings.TrimSpace(st.input)
			st.editing = false
			m.checkSettings()
		case "esc":
			st.editing = false
		default:
			st.input = editN(st.input, msg, 200)
		}
		return nil
	}
	switch key {
	case "up", "k", "shift+tab":
		st.cursor = (st.cursor + len(st.fields) - 1) % len(st.fields)
	case "down", "j", "tab":
		st.cursor = (st.cursor + 1) % len(st.fields)
	case "left", "right":
		// Across a paired row: the instance type and its node count.
		if f.group != "" {
			if key == "left" && st.cursor > 0 && st.fields[st.cursor-1].group == f.group {
				st.cursor--
			} else if key == "right" && st.cursor+1 < len(st.fields) && st.fields[st.cursor+1].group == f.group {
				st.cursor++
			}
		}
	case "m":
		m.notice = m.minimal()
		m.refreshFields()
		m.checkSettings()
	case "enter", " ":
		switch f.kind {
		case fToggle:
			if st.values[f.key] == "on" {
				st.values[f.key] = ""
			} else {
				st.values[f.key] = "on"
			}
		case fClass:
			if why := m.toggleClass(f.key); why != "" {
				m.notice = why
			}
		case fDatabase:
			if st.values[dbKey] == state.DatabaseRDS {
				st.values[dbKey] = state.DatabaseBundled
			} else {
				st.values[dbKey] = state.DatabaseRDS
			}
		default:
			if rel, by := m.settingsRelease(); f.key == "SUBSTRATE_TAG" && by != "" {
				m.notice = by + " installs " + rel.Tag + " (one release per install); turn it off to choose the release"
				break
			}
			if key == "enter" {
				st.editing, st.input = true, st.values[f.key]
			}
		}
		m.refreshFields()
		m.checkSettings()
	case "n":
		m.checkSettings()
		if len(st.errs) > 0 {
			m.notice = fmt.Sprintf("%d setting(s) need a fix before the review", len(st.errs))
			return nil
		}
		m.applySettings()
		return m.commit()
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	}
	return nil
}

func (m Model) settingsHints() []string {
	if m.set.editing {
		return []string{"enter save", "esc cancel"}
	}
	return []string{"↑/↓ move", "enter edit / toggle", "n review", "esc back", "q quit"}
}

func (m Model) settingsView() string {
	st := m.set
	total := m.bodyWidth() - 2
	rightW := max(total-54, 24)
	leftW := total - rightW - 2
	src := "built-in defaults (no config.env found)"
	if d := m.d.Config.Dir; d != "" {
		src = "defaults from " + tildify(filepath.Join(d, "config.env")) + " and config.local.env"
	}
	left := []string{theme.Title.Render("Settings") + "  " + theme.Subtle.Render(truncate(src, leftW-10))}
	if !m.s.ClusterIsNew {
		auto := "Auto Mode off"
		if m.s.AutoMode {
			auto = "Auto Mode on"
		}
		groups := "no node groups: Auto Mode nodes only"
		if g := m.s.NodeGroups(); len(g) > 0 {
			groups = "node groups: " + strings.Join(g, ", ")
		}
		left = append(left, "", theme.Title.Render("Cluster"),
			"  "+m.s.ClusterName+theme.Subtle.Render(fmt.Sprintf(" · EKS %s · %s", m.s.ClusterVersion, auto)),
			"  "+theme.Subtle.Render(truncate(groups, leftW-4)),
			theme.Fainted.Render(indent(lipgloss.NewStyle().Width(leftW-2).Render(
				"connected, not created: the new-cluster settings apply only to a cluster this run creates."), "  ")))
	}
	rel, relBy := m.settingsRelease()
	cell := func(i, w int) string {
		f := st.fields[i]
		shown := st.values[f.key]
		style := lipgloss.NewStyle()
		switch f.kind {
		case fToggle:
			shown = "off"
			if st.values[f.key] == "on" {
				shown = "on"
			}
		case fClass:
			box := "[ ]"
			switch {
			case st.classOn(f.key):
				box = "[" + theme.GlyphDone + "]"
			case m.classProblem(f.key) != "":
				box = "[–]"
			}
			shown = box + " " + f.label
		case fDatabase:
			shown = "Amazon RDS for PostgreSQL"
			if st.values[dbKey] == state.DatabaseBundled {
				shown = "Bundled PostgreSQL"
			}
		}
		if f.key == "SUBSTRATE_TAG" && relBy != "" {
			shown = rel.Tag + " (fork)"
		}
		if st.editing && i == st.cursor {
			in := []rune(st.input)
			if len(in) > w-3 {
				in = in[len(in)-(w-3):] // the end, where the cursor is
			}
			return theme.Selected.Render(pad(" "+string(in)+"▏", w))
		}
		if _, bad := st.errs[f.key]; bad {
			style = theme.Bad
		}
		if f.kind == fClass {
			if _, bad := st.errs[classKey]; bad && st.classOn(f.key) {
				style = theme.Bad
			}
		}
		if i == st.cursor {
			return theme.Selected.Render(pad(" "+truncate(shown, w-2), w))
		}
		return " " + style.Render(pad(truncate(shown, w-2), w-1))
	}
	section := ""
	for i := 0; i < len(st.fields); i++ {
		f := st.fields[i]
		if f.section != section {
			// A new cluster without node groups says so where their rows
			// would be.
			if f.section == "Every AWS resource" && m.s.ClusterIsNew && len(m.settingsGroups()) == 0 {
				left = append(left, "", theme.Title.Render("Node groups"), "  "+theme.Subtle.Render(truncate(noGroupsLine, leftW-4)))
			}
			section = f.section
			left = append(left, "", theme.Title.Render(section))
			if section == "Node groups" {
				left = append(left, theme.Fainted.Render("  group     instance type      nodes"))
			}
			if section == "Sandbox classes" {
				left = append(left, theme.Fainted.Render(truncate("  m: minimal, Auto Mode with the Nitro sandbox only", leftW)))
			}
		}
		switch {
		case f.kind == fClass:
			note := classNote[f.key]
			if m.classProblem(f.key) != "" {
				note = m.classShort(f.key)
			}
			left = append(left, " "+cell(i, 24)+" "+theme.Fainted.Render(truncate(note, leftW-27)))
		case f.group != "":
			note := ""
			if f.group == nodegroups.MicroVM {
				note = theme.Fainted.Render(" nested virt")
			}
			left = append(left, "  "+pad(f.group, 8)+cell(i, 18)+" "+cell(i+1, 5)+note)
			i++
		default:
			left = append(left, "  "+pad(f.label, 18)+cell(i, leftW-20))
		}
		if msg, bad := st.errs[f.key]; bad {
			left = append(left, "  "+pad("", 18)+theme.Bad.Render(truncate(msg, leftW-20)))
		}
		if f.key == state.ClassNitro {
			if msg, bad := st.errs[classKey]; bad {
				left = append(left, "  "+theme.Bad.Render(truncate(msg, leftW-2)))
			}
		}
	}

	f := st.fields[st.cursor]
	right := []string{theme.Title.Render("About this setting")}
	text := help[f.key]
	if doc := m.d.Config.Defaults.Doc(f.key); doc != "" {
		text = doc
	}
	if f.key == "SUBSTRATE_TAG" && relBy != "" {
		text = "The install runs " + rel.Tag + " from " + rel.Repo + ": " + relBy + " installs that fork's release, one release per install. " +
			"SUBSTRATE_TAG (" + st.values["SUBSTRATE_TAG"] + ") is kept for a run without it."
	}
	para := lipgloss.NewStyle().Width(rightW)
	right = append(right, theme.Subtle.Render(para.Render(joinParagraphs(text))))
	if f.kind == fText || f.kind == fToggle {
		right = append(right, "", theme.Fainted.Render(para.Render("Now: "+f.key+"="+st.values[f.key])))
		if def, ok := m.d.Config.Defaults.Get(f.key); ok {
			right = append(right, theme.Fainted.Render(para.Render("config.env: "+f.key+"="+def)))
		}
	}
	if f.kind == fClass {
		right = append(right, "", theme.Fainted.Render(para.Render("Now: "+classKey+"="+st.values[classKey])))
		if why := m.classProblem(f.key); why != "" {
			right = append(right, theme.Warning.Render(para.Render("Not available: "+why+".")))
		}
	}
	if st.editing {
		right = append(right, "", theme.Subtle.Render("Typing: enter saves, esc keeps the old value."))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftW).Render(strings.Join(left, "\n")), "  ",
		lipgloss.NewStyle().Width(rightW).Render(strings.Join(right, "\n")))
}

// joinParagraphs makes a config.env comment block flow: its lines join into
// one paragraph, and an indented line (a command, a link) is dropped, since
// the panel is narrow.
func joinParagraphs(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, " ") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(strings.Fields(strings.Join(out, " ")), " ")
}

// pad cuts or pads s (which may hold color codes) to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}
