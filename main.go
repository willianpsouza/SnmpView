package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gosnmp/gosnmp"
)

// Estilos
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00FF00")).
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#00FF00")).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#5555FF"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF0000")).
			Bold(true)

	warningStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFAA00")).
			Bold(true)

	successStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00FF00"))
)

// OIDs padrão para interfaces
const (
	oidIfIndex       = "1.3.6.1.2.1.2.2.1.1"
	oidIfDescr       = "1.3.6.1.2.1.2.2.1.2"
	oidIfType        = "1.3.6.1.2.1.2.2.1.3"
	oidIfAdminStatus = "1.3.6.1.2.1.2.2.1.7"
	oidIfOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	oidIfName        = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfAlias       = "1.3.6.1.2.1.31.1.1.1.18"
	oidIfHighSpeed   = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfHCInOctets  = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOutOctets = "1.3.6.1.2.1.31.1.1.1.10"
	oidIfInErrors    = "1.3.6.1.2.1.2.2.1.14"
	oidIfOutErrors   = "1.3.6.1.2.1.2.2.1.20"
	oidSysUpTime     = "1.3.6.1.2.1.1.3.0"
	oidSysName       = "1.3.6.1.2.1.1.5.0"
)

// ViewMode representa a tela atual
type ViewMode int

const (
	ViewDashboard ViewMode = iota
	ViewInterfaces
	ViewGraph
)

// SwitchConfig representa um switch no arquivo JSON
type SwitchConfig struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Community string `json:"community"`
}

// Config representa o arquivo de configuração
type Config struct {
	Switches []SwitchConfig `json:"switches"`
}

// SwitchSummary representa estatísticas de um switch
type SwitchSummary struct {
	Name           string
	Host           string
	Uptime         string
	ActivePorts    int
	TotalRxRate    float64
	TotalTxRate    float64
	TotalRxRateStr string
	TotalTxRateStr string
}

type InterfaceStats struct {
	Index       int
	Name        string
	Description string
	Alias       string
	Type        int
	Speed       uint64
	InOctets    uint64
	OutOctets   uint64
	InErrors    uint64
	OutErrors   uint64
	AdminStatus int
	OperStatus  int
	Timestamp   time.Time
}

type InterfaceMetrics struct {
	Index       int
	Description string
	Alias       string
	Type        string
	Speed       string
	RxRate      string
	TxRate      string
	RxRateBps   float64
	TxRateBps   float64
	ErrorRate   string
}

// DataPoint representa um ponto no gráfico
type DataPoint struct {
	Timestamp time.Time
	RxRate    float64
	TxRate    float64
}

type persistedHistory struct {
	Hosts map[string]map[string][]DataPoint `json:"hosts"`
}

type tickMsg time.Time

type model struct {
	// Configuração
	dashboardMode bool
	config        Config
	currentView   ViewMode

	// Dados do switch atual
	host       string
	community  string
	switchName string
	snmp       *gosnmp.GoSNMP

	// Tabelas
	dashboardTable  table.Model
	interfacesTable table.Model

	// Dados
	switches     []SwitchSummary
	prevStats    map[int]InterfaceStats
	currentStats map[int]InterfaceStats
	lastUpdate   time.Time
	sysName      string
	uptime       string
	alerts       []string

	// Gráfico
	selectedIfIndex int
	history         []DataPoint
	historyByIf     map[int][]DataPoint
	maxHistory      int
	historyPath     string
	historyTTL      time.Duration
	persisted       persistedHistory

	// Sistema
	err            error
	updateInterval time.Duration
}

func loadConfig(filename string) (Config, error) {
	var config Config
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(data, &config)
	return config, err
}

func initialModel(dashboardMode bool, host, community string) model {
	m := model{
		dashboardMode:  dashboardMode,
		host:           host,
		community:      community,
		prevStats:      make(map[int]InterfaceStats),
		currentStats:   make(map[int]InterfaceStats),
		updateInterval: 10 * time.Second,
		alerts:         []string{},
		maxHistory:     60, // 60 pontos de histórico (10 minutos a 10s cada)
		history:        []DataPoint{},
		historyByIf:    make(map[int][]DataPoint),
		historyTTL:     24 * time.Hour,
		historyPath:    filepath.Join(".", "snmpview_history.json"),
		persisted:      persistedHistory{Hosts: make(map[string]map[string][]DataPoint)},
	}

	m.loadPersistedHistory()

	if dashboardMode {
		m.currentView = ViewDashboard
		// Carregar configuração
		config, err := loadConfig("switches.json")
		if err != nil {
			m.err = fmt.Errorf("erro ao carregar switches.json: %v", err)
			return m
		}
		m.config = config

		// Configurar tabela do dashboard
		columns := []table.Column{
			{Title: "Switch", Width: 20},
			{Title: "Uptime", Width: 15},
			{Title: "Portas Ativas", Width: 15},
			{Title: "RX Total", Width: 15},
			{Title: "TX Total", Width: 15},
		}

		t := table.New(
			table.WithColumns(columns),
			table.WithFocused(true),
			table.WithHeight(15),
		)

		s := table.DefaultStyles()
		s.Header = s.Header.
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("#5555FF")).
			BorderBottom(true).
			Bold(true)
		s.Selected = s.Selected.
			Foreground(lipgloss.Color("#000000")).
			Background(lipgloss.Color("#00FF00")).
			Bold(false)
		t.SetStyles(s)

		m.dashboardTable = t
	} else {
		m.currentView = ViewInterfaces
		m.setupSNMP(host, community)
		m.hydrateHostHistory(host)
	}

	// Configurar tabela de interfaces
	columns := []table.Column{
		{Title: "Descrição", Width: 23},
		{Title: "Alias", Width: 40},
		{Title: "Tipo", Width: 12},
		{Title: "Speed", Width: 10},
		{Title: "RX Rate", Width: 12},
		{Title: "TX Rate", Width: 12},
		{Title: "Erros/s", Width: 10},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(25),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#5555FF")).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("#000000")).
		Background(lipgloss.Color("#00FF00")).
		Bold(false)
	t.SetStyles(s)

	m.interfacesTable = t

	return m
}

func (m *model) setupSNMP(host, community string) {
	m.host = host
	m.community = community
	m.hydrateHostHistory(host)
	m.snmp = &gosnmp.GoSNMP{
		Target:    host,
		Port:      161,
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   time.Duration(5) * time.Second,
		Retries:   2,
	}
}

func (m *model) loadPersistedHistory() {
	data, err := os.ReadFile(m.historyPath)
	if err != nil {
		return
	}

	var persisted persistedHistory
	if err := json.Unmarshal(data, &persisted); err != nil {
		return
	}
	if persisted.Hosts == nil {
		persisted.Hosts = make(map[string]map[string][]DataPoint)
	}
	m.persisted = persisted
}

func (m *model) hydrateHostHistory(host string) {
	m.historyByIf = make(map[int][]DataPoint)
	if host == "" || m.persisted.Hosts == nil {
		return
	}

	hostHistory, ok := m.persisted.Hosts[host]
	if !ok {
		return
	}

	cutoff := time.Now().Add(-m.historyTTL)
	for ifIndex, points := range hostHistory {
		idx, err := strconv.Atoi(ifIndex)
		if err != nil {
			continue
		}
		pruned := pruneHistory(points, cutoff)
		if len(pruned) > 0 {
			m.historyByIf[idx] = pruned
		}
	}
}

func (m *model) persistCurrentHostHistory() {
	if m.host == "" {
		return
	}
	if m.persisted.Hosts == nil {
		m.persisted.Hosts = make(map[string]map[string][]DataPoint)
	}
	if _, ok := m.persisted.Hosts[m.host]; !ok {
		m.persisted.Hosts[m.host] = make(map[string][]DataPoint)
	}

	cutoff := time.Now().Add(-m.historyTTL)
	m.persisted.Hosts[m.host] = make(map[string][]DataPoint)
	for ifIndex, points := range m.historyByIf {
		pruned := pruneHistory(points, cutoff)
		if len(pruned) == 0 {
			continue
		}
		m.historyByIf[ifIndex] = pruned
		m.persisted.Hosts[m.host][strconv.Itoa(ifIndex)] = pruned
	}

	encoded, err := json.MarshalIndent(m.persisted, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.historyPath, encoded, 0o644)
}

func pruneHistory(points []DataPoint, cutoff time.Time) []DataPoint {
	if len(points) == 0 {
		return points
	}
	firstValid := 0
	for i, p := range points {
		if p.Timestamp.After(cutoff) {
			firstValid = i
			break
		}
		if i == len(points)-1 {
			return []DataPoint{}
		}
	}
	return points[firstValid:]
}

func (m model) Init() tea.Cmd {
	if m.dashboardMode {
		return tea.Batch(
			tickCmd(m.updateInterval),
			m.fetchDashboardData,
		)
	}
	return tea.Batch(
		tickCmd(m.updateInterval),
		m.fetchData,
	)
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) fetchDashboardData() tea.Msg {
	defer func() {
		if r := recover(); r != nil {
			m.err = fmt.Errorf("panic durante coleta do dashboard: %v", r)
		}
	}()

	var summaries []SwitchSummary

	for _, sw := range m.config.Switches {
		summary := SwitchSummary{
			Name: sw.Name,
			Host: sw.Host,
		}

		// Conectar ao switch
		snmp := &gosnmp.GoSNMP{
			Target:    sw.Host,
			Port:      161,
			Community: sw.Community,
			Version:   gosnmp.Version2c,
			Timeout:   time.Duration(3) * time.Second,
			Retries:   1,
		}

		err := snmp.Connect()
		if err != nil {
			summary.Uptime = "offline"
			summaries = append(summaries, summary)
			continue
		}
		defer snmp.Conn.Close()

		// Obter uptime
		result, err := snmp.Get([]string{oidSysUpTime})
		if err == nil && len(result.Variables) > 0 {
			ticks := toUint32(result.Variables[0].Value)
			summary.Uptime = formatUptime(ticks)
		}

		// Obter interfaces e contar portas ativas
		var activeCount int
		interfaces := m.getInterfacesForSwitch(snmp)
		activeCount = len(interfaces)

		summary.ActivePorts = activeCount
		summary.TotalRxRateStr = "-"
		summary.TotalTxRateStr = "-"

		summaries = append(summaries, summary)
	}

	m.switches = summaries
	m.lastUpdate = time.Now()
	return m
}

func (m model) fetchData() tea.Msg {
	defer func() {
		if r := recover(); r != nil {
			m.err = fmt.Errorf("panic durante coleta SNMP: %v", r)
		}
	}()

	// Conectar ao switch
	if err := m.snmp.Connect(); err != nil {
		return err
	}
	defer func() { _ = m.snmp.Conn.Close() }()

	// Obter nome do sistema
	result, err := m.snmp.Get([]string{oidSysName})
	if err == nil && len(result.Variables) > 0 {
		m.sysName = toString(result.Variables[0].Value)
	}

	// Obter uptime
	result, err = m.snmp.Get([]string{oidSysUpTime})
	if err == nil && len(result.Variables) > 0 {
		ticks := toUint32(result.Variables[0].Value)
		m.uptime = formatUptime(ticks)
	}

	// Obter lista de interfaces
	interfaces := m.getInterfaces()

	// Obter estatísticas de cada interface
	stats := make(map[int]InterfaceStats)
	for _, ifIndex := range interfaces {
		stat := m.getInterfaceStats(ifIndex)
		if stat.Name != "" || stat.Description != "" {
			stats[ifIndex] = stat
		}
	}

	m.currentStats = stats
	m.lastUpdate = time.Now()

	return m
}

func (m model) getInterfacesForSwitch(snmp *gosnmp.GoSNMP) []int {
	var allInterfaces []int

	err := snmp.Walk(oidIfIndex, func(pdu gosnmp.SnmpPDU) error {
		if pdu.Value != nil {
			allInterfaces = append(allInterfaces, toInt(pdu.Value))
		}
		return nil
	})

	if err != nil {
		return allInterfaces
	}

	var filteredInterfaces []int
	allowedTypes := map[int]bool{
		6:   true,
		117: true,
		161: true,
	}

	for _, ifIndex := range allInterfaces {
		oids := []string{
			fmt.Sprintf("%s.%d", oidIfType, ifIndex),
			fmt.Sprintf("%s.%d", oidIfAdminStatus, ifIndex),
		}
		result, err := snmp.Get(oids)
		if err == nil && len(result.Variables) == 2 {
			ifType := toInt(result.Variables[0].Value)
			ifAdminStatus := toInt(result.Variables[1].Value)

			if allowedTypes[ifType] && ifAdminStatus == 1 {
				filteredInterfaces = append(filteredInterfaces, ifIndex)
			}
		}
	}

	return filteredInterfaces
}

func (m model) getInterfaces() []int {
	return m.getInterfacesForSwitch(m.snmp)
}

func (m model) getInterfaceStatsForSwitch(snmp *gosnmp.GoSNMP, ifIndex int) InterfaceStats {
	stat := InterfaceStats{
		Index:     ifIndex,
		Timestamp: time.Now(),
	}

	oids := []string{
		fmt.Sprintf("%s.%d", oidIfDescr, ifIndex),
		fmt.Sprintf("%s.%d", oidIfName, ifIndex),
		fmt.Sprintf("%s.%d", oidIfAlias, ifIndex),
		fmt.Sprintf("%s.%d", oidIfType, ifIndex),
		fmt.Sprintf("%s.%d", oidIfHighSpeed, ifIndex),
		fmt.Sprintf("%s.%d", oidIfHCInOctets, ifIndex),
		fmt.Sprintf("%s.%d", oidIfHCOutOctets, ifIndex),
		fmt.Sprintf("%s.%d", oidIfInErrors, ifIndex),
		fmt.Sprintf("%s.%d", oidIfOutErrors, ifIndex),
		fmt.Sprintf("%s.%d", oidIfAdminStatus, ifIndex),
		fmt.Sprintf("%s.%d", oidIfOperStatus, ifIndex),
	}

	result, err := snmp.Get(oids)
	if err != nil {
		return stat
	}

	for i, variable := range result.Variables {
		switch i {
		case 0:
			if variable.Value != nil {
				stat.Description = toString(variable.Value)
			}
		case 1:
			if variable.Value != nil {
				stat.Name = toString(variable.Value)
			}
		case 2:
			if variable.Value != nil {
				stat.Alias = toString(variable.Value)
			}
		case 3:
			if variable.Value != nil {
				stat.Type = toInt(variable.Value)
			}
		case 4:
			if variable.Value != nil {
				stat.Speed = toUint64(variable.Value)
			}
		case 5:
			if variable.Value != nil {
				stat.InOctets = toUint64(variable.Value)
			}
		case 6:
			if variable.Value != nil {
				stat.OutOctets = toUint64(variable.Value)
			}
		case 7:
			if variable.Value != nil {
				stat.InErrors = toUint64(variable.Value)
			}
		case 8:
			if variable.Value != nil {
				stat.OutErrors = toUint64(variable.Value)
			}
		case 9:
			if variable.Value != nil {
				stat.AdminStatus = toInt(variable.Value)
			}
		case 10:
			if variable.Value != nil {
				stat.OperStatus = toInt(variable.Value)
			}
		}
	}

	return stat
}

func (m model) getInterfaceStats(ifIndex int) InterfaceStats {
	return m.getInterfaceStatsForSwitch(m.snmp, ifIndex)
}

func (m *model) calculateMetrics() []InterfaceMetrics {
	var metrics []InterfaceMetrics
	m.alerts = []string{}
	historyChanged := false

	var indices []int
	for ifIndex := range m.currentStats {
		indices = append(indices, ifIndex)
	}
	sort.Ints(indices)

	for _, ifIndex := range indices {
		current := m.currentStats[ifIndex]

		metric := InterfaceMetrics{
			Index:       ifIndex,
			Description: truncateString(current.Description, 22),
			Alias:       truncateString(current.Alias, 39),
			Type:        getInterfaceType(current.Type),
			Speed:       formatSpeed(current.Speed),
			RxRate:      "0 bps",
			TxRate:      "0 bps",
			RxRateBps:   0,
			TxRateBps:   0,
			ErrorRate:   "0.00",
		}

		if prev, ok := m.prevStats[ifIndex]; ok {
			timeDiff := current.Timestamp.Sub(prev.Timestamp).Seconds()
			if timeDiff > 0 {
				rxBytes := float64(counterDelta(current.InOctets, prev.InOctets))
				rxRate := (rxBytes * 8) / timeDiff
				metric.RxRate = formatBps(rxRate)
				metric.RxRateBps = rxRate

				txBytes := float64(counterDelta(current.OutOctets, prev.OutOctets))
				txRate := (txBytes * 8) / timeDiff
				metric.TxRate = formatBps(txRate)
				metric.TxRateBps = txRate

				inErr := float64(counterDelta(current.InErrors, prev.InErrors))
				outErr := float64(counterDelta(current.OutErrors, prev.OutErrors))
				errorRate := (inErr + outErr) / timeDiff
				metric.ErrorRate = fmt.Sprintf("%.2f", errorRate)

				if errorRate > 1.0 {
					interfaceName := current.Name
					if interfaceName == "" {
						interfaceName = current.Description
					}
					m.alerts = append(m.alerts,
						fmt.Sprintf("⚠️  %s: %.2f erros/s detectados", interfaceName, errorRate))
				}

				if current.Speed > 0 {
					speedBps := float64(current.Speed) * 1000000
					usage := (rxRate / speedBps) * 100
					if usage > 80 {
						interfaceName := current.Name
						if interfaceName == "" {
							interfaceName = current.Description
						}
						m.alerts = append(m.alerts,
							fmt.Sprintf("📊 %s: %.1f%% de utilização RX", interfaceName, usage))
					}
				}

				// Adicionar ao histórico se esta interface está selecionada e estamos na view de gráfico
				point := DataPoint{Timestamp: time.Now(), RxRate: rxRate, TxRate: txRate}
				history := append(m.historyByIf[ifIndex], point)
				m.historyByIf[ifIndex] = pruneHistory(history, time.Now().Add(-m.historyTTL))
				historyChanged = true
			}
		}

		metrics = append(metrics, metric)
	}

	if historyChanged {
		m.persistCurrentHostHistory()
	}

	if m.currentView == ViewGraph {
		m.history = append([]DataPoint(nil), m.historyByIf[m.selectedIfIndex]...)
		if len(m.history) > m.maxHistory {
			m.history = m.history[len(m.history)-m.maxHistory:]
		}
	}

	return metrics
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			// Q sempre sai
			return m, tea.Quit
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			// ESC volta para view anterior
			switch m.currentView {
			case ViewGraph:
				m.currentView = ViewInterfaces
				m.history = []DataPoint{} // Limpar histórico
				return m, nil
			case ViewInterfaces:
				if m.dashboardMode {
					m.currentView = ViewDashboard
					m.prevStats = make(map[int]InterfaceStats)
					m.currentStats = make(map[int]InterfaceStats)
					return m, m.fetchDashboardData
				}
				return m, tea.Quit
			case ViewDashboard:
				return m, tea.Quit
			}
		case "r":
			// Reset apenas funciona na view de interfaces
			if m.currentView == ViewInterfaces {
				m.prevStats = make(map[int]InterfaceStats)
				return m, m.fetchData
			}
		case "enter":
			// ENTER muda de view
			switch m.currentView {
			case ViewDashboard:
				// Selecionar switch
				selectedIdx := m.dashboardTable.Cursor()
				if selectedIdx < len(m.switches) {
					sw := m.config.Switches[selectedIdx]
					m.switchName = sw.Name
					m.setupSNMP(sw.Host, sw.Community)
					m.currentView = ViewInterfaces
					m.prevStats = make(map[int]InterfaceStats)
					m.currentStats = make(map[int]InterfaceStats)
					return m, m.fetchData
				}
			case ViewInterfaces:
				// Selecionar interface para ver gráfico
				selectedIdx := m.interfacesTable.Cursor()
				metrics := m.calculateMetrics()
				if selectedIdx < len(metrics) {
					m.selectedIfIndex = metrics[selectedIdx].Index
					m.currentView = ViewGraph
					m.history = append([]DataPoint(nil), m.historyByIf[m.selectedIfIndex]...)
					if len(m.history) > m.maxHistory {
						m.history = m.history[len(m.history)-m.maxHistory:]
					}
					return m, nil
				}
			}
		case "up", "down":
			// Navegação nas tabelas
			switch m.currentView {
			case ViewDashboard:
				m.dashboardTable, cmd = m.dashboardTable.Update(msg)
				return m, cmd
			case ViewInterfaces:
				m.interfacesTable, cmd = m.interfacesTable.Update(msg)
				return m, cmd
			}
		}

	case tickMsg:
		switch m.currentView {
		case ViewDashboard:
			return m, tea.Batch(
				tickCmd(m.updateInterval),
				m.fetchDashboardData,
			)
		case ViewInterfaces, ViewGraph:
			return m, tea.Batch(
				tickCmd(m.updateInterval),
				m.fetchData,
			)
		}

	case model:
		if m.currentView == ViewDashboard {
			// Atualizar dashboard
			m.switches = msg.switches
			m.lastUpdate = msg.lastUpdate

			rows := make([]table.Row, len(m.switches))
			for i, sw := range m.switches {
				rows[i] = table.Row{
					sw.Name,
					sw.Uptime,
					fmt.Sprintf("%d", sw.ActivePorts),
					sw.TotalRxRateStr,
					sw.TotalTxRateStr,
				}
			}
			m.dashboardTable.SetRows(rows)
		} else {
			// Atualizar interfaces
			m.prevStats = make(map[int]InterfaceStats)
			for k, v := range m.currentStats {
				m.prevStats[k] = v
			}

			m.currentStats = msg.currentStats
			m.sysName = msg.sysName
			m.uptime = msg.uptime
			m.lastUpdate = msg.lastUpdate

			metrics := m.calculateMetrics()
			rows := make([]table.Row, len(metrics))
			for i, metric := range metrics {
				rows[i] = table.Row{
					metric.Description,
					metric.Alias,
					metric.Type,
					metric.Speed,
					metric.RxRate,
					metric.TxRate,
					metric.ErrorRate,
				}
			}
			m.interfacesTable.SetRows(rows)
		}

		return m, nil

	case error:
		m.err = msg
		return m, nil
	}

	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Erro: %v\n\nPressione 'q' para sair.", m.err))
	}

	switch m.currentView {
	case ViewDashboard:
		return m.viewDashboard()
	case ViewInterfaces:
		return m.viewInterfaces()
	case ViewGraph:
		return m.viewGraph()
	}

	return ""
}

func (m model) viewDashboard() string {
	var b strings.Builder

	title := "SWITCH MONITOR - DASHBOARD"
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n\n")

	statusLine := fmt.Sprintf("Última atualização: %s  |  Intervalo: %v  |  Switches: %d",
		m.lastUpdate.Format("15:04:05"),
		m.updateInterval,
		len(m.switches))
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Render(statusLine))
	b.WriteString("\n\n")

	b.WriteString(m.dashboardTable.View())
	b.WriteString("\n\n")

	footer := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#666666")).
		Render("[↑↓] Navegar  [ENTER] Selecionar Switch  [Q] Sair")
	b.WriteString(footer)

	return b.String()
}

func (m model) viewInterfaces() string {
	var b strings.Builder

	title := fmt.Sprintf("SWITCH MONITOR - %s (%s)", m.host, m.sysName)
	if m.switchName != "" {
		title = fmt.Sprintf("SWITCH MONITOR - %s (%s)", m.switchName, m.host)
	}
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n\n")

	statusLine := fmt.Sprintf("Última atualização: %s  |  Intervalo: %v  |  Uptime: %s",
		m.lastUpdate.Format("15:04:05"),
		m.updateInterval,
		m.uptime)
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Render(statusLine))
	b.WriteString("\n\n")

	b.WriteString(m.interfacesTable.View())
	b.WriteString("\n\n")

	if len(m.alerts) > 0 {
		alertBox := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FFAA00")).
			Padding(0, 1).
			Width(200)

		alertContent := "ALERTAS\n"
		for _, alert := range m.alerts {
			alertContent += alert + "\n"
		}
		b.WriteString(alertBox.Render(alertContent))
		b.WriteString("\n\n")
	}

	footer := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#666666"))

	var footerText string
	if m.dashboardMode {
		footerText = footer.Render("[↑↓] Navegar  [ENTER] Ver Gráfico  [R] Resetar  [ESC] Voltar  [Q] Sair")
	} else {
		footerText = footer.Render("[↑↓] Navegar  [ENTER] Ver Gráfico  [R] Resetar  [Q] Sair")
	}
	b.WriteString(footerText)

	return b.String()
}

func (m model) viewGraph() string {
	var b strings.Builder

	// Obter dados da interface selecionada
	stat, ok := m.currentStats[m.selectedIfIndex]
	if !ok {
		return errorStyle.Render("Interface não encontrada")
	}

	title := fmt.Sprintf("GRÁFICO - %s (%s)", stat.Description, stat.Alias)
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n\n")

	// Info da interface
	info := fmt.Sprintf("Speed: %s  |  Tipo: %s  |  Pontos: %d/%d",
		formatSpeed(stat.Speed),
		getInterfaceType(stat.Type),
		len(m.history),
		m.maxHistory)
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Render(info))
	b.WriteString("\n\n")

	// Renderizar gráfico
	if len(m.history) < 2 {
		b.WriteString(warningStyle.Render("Coletando dados... Aguarde algumas atualizações."))
		b.WriteString("\n\n")
	} else {
		graph := m.renderGraph(stat.Speed)
		b.WriteString(graph)
		b.WriteString("\n\n")
	}

	// Legenda
	currentRx := "0 bps"
	currentTx := "0 bps"
	if len(m.history) > 0 {
		last := m.history[len(m.history)-1]
		currentRx = formatBps(last.RxRate)
		currentTx = formatBps(last.TxRate)
	}

	legend := fmt.Sprintf("🔵 RX: %s  |  🔴 TX: %s", currentRx, currentTx)
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF")).Render(legend))
	b.WriteString("\n\n")

	footer := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#666666")).
		Render("[ESC] Voltar  [Q] Sair")
	b.WriteString(footer)

	return b.String()
}

func (m model) renderGraph(speedMbps uint64) string {
	if len(m.history) == 0 {
		return ""
	}

	const height = 20
	const width = 100

	// Escala dinâmica com margem visual para lembrar o estilo do btop.
	interfaceLimit := float64(speedMbps) * 1000000 // Mbps para bps
	if interfaceLimit <= 0 {
		interfaceLimit = 1000000
	}

	peak := 0.0
	for _, p := range m.history {
		peak = math.Max(peak, math.Max(p.RxRate, p.TxRate))
	}
	maxRate := math.Max(interfaceLimit*0.05, peak*1.20)
	maxRate = math.Min(maxRate, interfaceLimit)

	// Criar grid
	grid := make([][]rune, height)
	for i := range grid {
		grid[i] = make([]rune, width)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}

	// Plotar dados
	pointsToShow := len(m.history)
	if pointsToShow > width {
		pointsToShow = width
	}

	startIdx := len(m.history) - pointsToShow

	rxSeries := make([]float64, pointsToShow)
	txSeries := make([]float64, pointsToShow)
	for i := 0; i < pointsToShow; i++ {
		point := m.history[startIdx+i]
		rxSeries[i] = point.RxRate
		txSeries[i] = point.TxRate
	}

	rxSeries = smoothSeries(rxSeries, 0.35)
	txSeries = smoothSeries(txSeries, 0.35)

	half := height / 2
	for x := 0; x < pointsToShow; x++ {
		rxValue := math.Max(rxSeries[x], 0)
		txValue := math.Max(txSeries[x], 0)

		rxUnits := int(math.Round((rxValue / maxRate) * float64(half*8)))
		txUnits := int(math.Round((txValue / maxRate) * float64(half*8)))

		drawBar(grid, x, half-1, -1, rxUnits, 'R')
		drawBar(grid, x, half, 1, txUnits, 'T')
	}

	for x := 0; x < width; x++ {
		if grid[half][x] == ' ' {
			grid[half][x] = '─'
		}
	}

	// Renderizar grid com bordas e escala
	var output strings.Builder

	// Linha superior
	output.WriteString("┌")
	output.WriteString(strings.Repeat("─", width))
	output.WriteString("┐\n")

	// Linhas do gráfico com escala
	for i := 0; i < height; i++ {
		// Escala lateral
		percent := 100 - (i * 100 / height)
		label := fmt.Sprintf("%3d%%", percent)
		output.WriteString(label)
		output.WriteString("│")

		// Colorir linha
		line := string(grid[i])
		// RX = azul, TX = vermelho, ambos = magenta
		line = strings.ReplaceAll(line, "R", lipgloss.NewStyle().Foreground(lipgloss.Color("#3BA1FF")).Render("█"))
		line = strings.ReplaceAll(line, "T", lipgloss.NewStyle().Foreground(lipgloss.Color("#D34BFF")).Render("█"))
		line = strings.ReplaceAll(line, "─", lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Render("─"))

		output.WriteString(line)
		output.WriteString("│\n")
	}

	// Linha inferior
	output.WriteString("    └")
	output.WriteString(strings.Repeat("─", width))
	output.WriteString("┘\n")

	// Eixo de tempo
	timeLabel := fmt.Sprintf("     ← %d segundos atrás", len(m.history)*10)
	output.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Render(timeLabel))
	output.WriteString(strings.Repeat(" ", width-len(timeLabel)-10))
	output.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Render("agora →"))

	return output.String()
}

func drawBar(grid [][]rune, x int, startRow int, direction int, units int, marker rune) {
	if len(grid) == 0 || x < 0 || x >= len(grid[0]) || units <= 0 {
		return
	}

	height := len(grid)
	filled := units
	for step := 0; filled > 0; step++ {
		row := startRow + (step * direction)
		if row < 0 || row >= height {
			break
		}
		if filled > 0 {
			grid[row][x] = marker
		}
		filled -= 8
	}
}

// Funções auxiliares
func formatSpeed(mbps uint64) string {
	if mbps >= 1000 {
		return fmt.Sprintf("%d Gbps", mbps/1000)
	}
	return fmt.Sprintf("%d Mbps", mbps)
}

func formatBps(bps float64) string {
	if math.IsNaN(bps) || math.IsInf(bps, 0) {
		return "0 bps"
	}

	units := []string{"bps", "Kbps", "Mbps", "Gbps", "Tbps"}
	unitIndex := 0

	for bps >= 1000 && unitIndex < len(units)-1 {
		bps /= 1000
		unitIndex++
	}

	return fmt.Sprintf("%.2f %s", bps, units[unitIndex])
}

func formatUptime(ticks uint32) string {
	seconds := ticks / 100
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60

	return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
}

func getStatusString(status int) string {
	switch status {
	case 1:
		return "up"
	case 2:
		return "down"
	case 3:
		return "testing"
	default:
		return "unknown"
	}
}

func getInterfaceType(ifType int) string {
	types := map[int]string{
		6:   "Ethernet",
		117: "GigE",
		24:  "Loopback",
		53:  "Virtual",
		131: "Tunnel",
		135: "VLAN",
		136: "IP VLAN",
		161: "LAG",
	}

	if typeName, ok := types[ifType]; ok {
		return typeName
	}

	return fmt.Sprintf("Type-%d", ifType)
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func smoothSeries(series []float64, alpha float64) []float64 {
	if len(series) == 0 {
		return series
	}
	out := make([]float64, len(series))
	out[0] = series[0]
	for i := 1; i < len(series); i++ {
		out[i] = alpha*series[i] + (1-alpha)*out[i-1]
	}
	return out
}

func drawSeriesLine(grid [][]rune, x0 int, y0Value float64, x1 int, y1Value float64, maxRate float64, symbol rune) {
	height := len(grid)
	if height == 0 {
		return
	}
	width := len(grid[0])
	y0 := rateToY(y0Value, maxRate, height)
	y1 := rateToY(y1Value, maxRate, height)
	dx := x1 - x0
	if dx <= 0 {
		setGraphPoint(grid, x0, y0, symbol, width, height)
		return
	}

	for step := 0; step <= dx; step++ {
		t := float64(step) / float64(dx)
		x := x0 + step
		y := int(math.Round(float64(y0) + t*float64(y1-y0)))
		setGraphPoint(grid, x, y, symbol, width, height)
	}
}

func setGraphPoint(grid [][]rune, x, y int, symbol rune, width, height int) {
	if x < 0 || x >= width || y < 0 || y >= height {
		return
	}
	if grid[y][x] != ' ' && grid[y][x] != symbol {
		grid[y][x] = '▓'
		return
	}
	grid[y][x] = symbol
}

func rateToY(rate, maxRate float64, height int) int {
	if maxRate <= 0 {
		return height - 1
	}
	if rate < 0 {
		rate = 0
	}
	percent := math.Min(rate/maxRate, 1)
	return height - 1 - int(percent*float64(height-1))
}

func toBigInt(value interface{}) *big.Int {
	if value == nil {
		return big.NewInt(0)
	}
	if v := gosnmp.ToBigInt(value); v != nil {
		return v
	}
	return big.NewInt(0)
}

func toUint64(value interface{}) uint64 {
	v := toBigInt(value)
	if v.Sign() < 0 {
		return 0
	}
	return v.Uint64()
}

func toUint32(value interface{}) uint32 {
	return uint32(toUint64(value))
}

func toInt(value interface{}) int {
	return int(toUint64(value))
}

func toString(value interface{}) string {
	switch v := value.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func counterDelta(current, previous uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	return 0
}

func promptForTarget(reader *bufio.Reader) (string, string, error) {
	fmt.Print("Host/IP do switch: ")
	host, err := reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}

	fmt.Print("Community SNMP: ")
	community, err := reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}

	host = strings.TrimSpace(host)
	community = strings.TrimSpace(community)

	if host == "" || community == "" {
		return "", "", fmt.Errorf("host e community são obrigatórios")
	}

	return host, community, nil
}

func main() {
	dashboardMode := flag.Bool("d", false, "Dashboard mode - load switches from switches.json")
	hostFlag := flag.String("host", "", "Host/IP do switch")
	communityFlag := flag.String("community", "", "Community SNMP")
	flag.Parse()

	var m model

	if *dashboardMode {
		m = initialModel(true, "", "")
	} else {
		host := strings.TrimSpace(*hostFlag)
		community := strings.TrimSpace(*communityFlag)

		if host == "" || community == "" {
			reader := bufio.NewReader(os.Stdin)
			var err error
			host, community, err = promptForTarget(reader)
			if err != nil {
				fmt.Printf("Erro ao ler host/community: %v\n", err)
				os.Exit(1)
			}
		}
		m = initialModel(false, host, community)
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Erro: %v\n", err)
		os.Exit(1)
	}
}
