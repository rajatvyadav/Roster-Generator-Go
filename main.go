package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

const (
	DefaultDays = 30

	// Common layouts / filenames
	DateLayoutYMD      = "2006-01-02"
	DefaultConfigFile  = "roster_config.json"
	DefaultLegacyState = "roster_state.json"

	// Shift codes
	ShiftD1    = "D1"
	ShiftD3    = "D3"
	ShiftD5    = "D5"
	ShiftOff   = "OFF"
	ShiftLeave = "L"

	// Sheet names
	SheetRoster    = "Roster Schedule"
	SheetDetails   = "Shift Details"
	SheetEmployees = "Employee Master"

	// Console prompts
	PromptEmployees = "Enter number of employees"
	PromptDays      = "Enter number of days to generate"
	PromptStartDate = "Enter start date (YYYY-MM-DD) (press Enter for tomorrow)"
	PromptOutput    = "Enter output filename (.xlsx) (press Enter for auto)"

	// Banner strings
	BannerLine1 = "=============================================="
	BannerTitle = "        24/7 Employee Shift Roster Tool       "
	BannerBy    = "                Creator: Shreya               "
)

var (
	ShiftOrder = []string{ShiftD1, ShiftD3, ShiftD5}
)

type ShiftDef struct {
	Name  string
	Time  string
	Color string
}

var Shifts = map[string]ShiftDef{
	ShiftD1:    {Name: "Morning", Time: "6AM-2PM", Color: "92D050"},
	ShiftD3:    {Name: "Afternoon", Time: "2PM-10PM", Color: "FFFF00"},
	ShiftD5:    {Name: "Night", Time: "10PM-6AM", Color: "5B9BD5"},
	ShiftOff:   {Name: "Off", Time: "-", Color: "BFBFBF"},
	ShiftLeave: {Name: "Leave", Time: "-", Color: "F8CBAD"},
}

type EmployeeConfig struct {
	Name   string        `json:"name"`
	Active bool          `json:"active"`
	Leaves []LeavePeriod `json:"leaves"`
	// Continuity is maintained by the tool so rosters can continue across months.
	Continuity *EmployeeContinuity `json:"continuity,omitempty"`
}

type LeavePeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type EmployeeContinuity struct {
	Shift      string `json:"shift"`    // D1, D3, D5
	CycleDay   int    `json:"cycleDay"` // 0..6 where 0-4=work, 5-6=off
	CycleIndex int    `json:"cycleIndex,omitempty"`
}

// RosterFile is the single JSON file that holds everything:
// - employees + leaves (human-editable)
// - nextRosterStartDate + continuity (auto-managed)
type RosterFile struct {
	NextRosterStartDate string `json:"nextRosterStartDate,omitempty"` // YYYY-MM-DD
	// If true, shift is picked pseudo-randomly each cycle (instead of rotating D1→D3→D5).
	RandomizeShifts bool             `json:"randomizeShifts"`
	Employees       []EmployeeConfig `json:"employees"`
}

type EmployeeState struct {
	ShiftIndex int `json:"shiftIndex"`
	CycleDay   int `json:"cycleDay"` // 0..6, where 0-4=work, 5-6=off
	CycleIndex int `json:"cycleIndex,omitempty"`
}

type legacyRosterState struct {
	NextDate  string                   `json:"nextDate"` // YYYY-MM-DD
	Employees map[string]EmployeeState `json:"employees"`
}

func main() {
	configPath := flag.String("config", DefaultConfigFile, "Roster JSON (employees, leaves, and continuity). This file is updated after generation")
	employees := flag.Int("employees", 20, "Number of employees (default: 20)")
	days := flag.Int("days", DefaultDays, "Number of days to generate (default: 42)")
	startDateStr := flag.String("start-date", "", "Optional start date (YYYY-MM-DD). If omitted, uses tomorrow.")
	output := flag.String("output", "", "Optional output filename (.xlsx). If omitted, auto-generates a unique name.")
	legacyStateFile := flag.String("state-file", DefaultLegacyState, "(Deprecated) legacy state file; used only for one-time migration")
	resetState := flag.Bool("reset-state", false, "Ignore any existing state file and start fresh")
	flag.Parse()

	printBanner()

	reader := bufio.NewReader(os.Stdin)
	roster, err := loadRosterFile(*configPath)
	if err != nil {
		fatal(err)
	}
	if roster == nil {
		roster = &RosterFile{}
	}

	// If this is an old-style config (employees only) and a legacy state file exists, migrate.
	if strings.TrimSpace(roster.NextRosterStartDate) == "" {
		legacy, err := loadLegacyRosterState(*legacyStateFile)
		if err != nil {
			fatal(err)
		}
		if legacy != nil && len(legacy.Employees) > 0 {
			roster.NextRosterStartDate = strings.TrimSpace(legacy.NextDate)
			for i := range roster.Employees {
				name := strings.TrimSpace(roster.Employees[i].Name)
				if name == "" {
					continue
				}
				if roster.Employees[i].Continuity != nil {
					continue
				}
				if st, ok := legacy.Employees[name]; ok {
					roster.Employees[i].Continuity = &EmployeeContinuity{
						Shift:    ShiftOrder[st.ShiftIndex%len(ShiftOrder)],
						CycleDay: st.CycleDay,
					}
				}
			}
		}
	}

	var activeEmployees []EmployeeConfig
	if roster != nil && len(roster.Employees) > 0 {
		activeEmployees = filterActiveEmployees(roster.Employees)
		if len(activeEmployees) == 0 {
			fatal(fmt.Errorf("config %s has no active employees", *configPath))
		}
		fmt.Printf("Using config: %s (active employees: %d)\n", *configPath, len(activeEmployees))
	} else {
		count := promptInt(reader, PromptEmployees, *employees)
		if count < 1 {
			count = 1
		}
		activeEmployees = buildDefaultEmployees(count)
		roster.Employees = append([]EmployeeConfig(nil), activeEmployees...)
	}

	totalDays := promptInt(reader, PromptDays, *days)
	startDateInput := promptString(reader, PromptStartDate, *startDateStr)
	outPath := promptString(reader, PromptOutput, *output)
	if totalDays < 1 {
		totalDays = 1
	}

	start, err := resolveStartDate(startDateInput)
	if err != nil {
		fatal(err)
	}
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())

	seedStates := buildSeedStates(activeEmployees)
	alignedStates, err := alignStatesForStart(*resetState, roster, activeEmployees, seedStates, start, roster.RandomizeShifts)
	if err != nil {
		fatal(err)
	}
	schedules := make([][]string, 0, len(activeEmployees))
	endStates := make([]EmployeeState, 0, len(activeEmployees))
	for i, cfg := range activeEmployees {
		sched, endState := buildEmployeeSchedule(totalDays, start, cfg.Name, alignedStates[i], cfg.Leaves, roster.RandomizeShifts)
		schedules = append(schedules, sched)
		endStates = append(endStates, endState)
		updateEmployeeContinuity(roster, cfg.Name, endState)
	}
	end := start.AddDate(0, 0, totalDays-1)
	roster.NextRosterStartDate = end.AddDate(0, 0, 1).Format(DateLayoutYMD)
	if err := ensureMinDailyCoverage(schedules, 1); err != nil {
		fmt.Printf("WARNING: %v\n", err)
	}
	if err := saveRosterFile(*configPath, roster); err != nil {
		fatal(err)
	}

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	if err := createRosterSheet(f, start, totalDays, activeEmployees, schedules); err != nil {
		fatal(err)
	}
	if err := createShiftDetailsSheet(f, start, totalDays, len(activeEmployees)); err != nil {
		fatal(err)
	}
	if err := createEmployeeMasterSheet(f, totalDays, activeEmployees, schedules); err != nil {
		fatal(err)
	}

	if outPath == "" {
		stamp := time.Now().Format("20060102_150405")
		outPath = fmt.Sprintf("Employee_Shift_Roster_%dEmployees_%s.xlsx", len(activeEmployees), stamp)
	}
	if !strings.HasSuffix(strings.ToLower(outPath), ".xlsx") {
		outPath += ".xlsx"
	}

	if err := f.SaveAs(outPath); err != nil {
		fatal(err)
	}

	abs, _ := filepath.Abs(outPath)
	fmt.Printf("✅ Excel file created successfully: %s\n", abs)
	fmt.Printf("Schedule Period: %s - %s\n", start.Format("02 Jan 2006"), end.Format("02 Jan 2006"))
	fmt.Printf("Employees: %d\n", len(activeEmployees))
	fmt.Printf("Config updated: %s (nextRosterStartDate=%s)\n", *configPath, roster.NextRosterStartDate)
}

func printBanner() {
	fmt.Println(BannerLine1)
	fmt.Println(BannerTitle)
	fmt.Println(BannerBy)
	fmt.Println(BannerLine1)
}

func promptString(reader *bufio.Reader, label string, def string) string {
	if strings.TrimSpace(def) != "" {
		fmt.Printf("%s [default: %s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func promptInt(reader *bufio.Reader, label string, def int) int {
	for {
		fmt.Printf("%s [default: %d]: ", label, def)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		v, err := strconv.Atoi(line)
		if err != nil || v < 1 {
			fmt.Println("Please enter a valid positive integer.")
			continue
		}
		return v
	}
}

func fatal(err error) {
	fmt.Printf("ERROR: %v\n", err)
	panic(err)
}

func mustNewStyle(f *excelize.File, style *excelize.Style) int {
	id, err := f.NewStyle(style)
	if err != nil {
		panic(err)
	}
	return id
}

func resolveStartDate(s string) (time.Time, error) {
	if strings.TrimSpace(s) != "" {
		t, err := time.Parse(DateLayoutYMD, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --start-date (expected YYYY-MM-DD): %w", err)
		}
		return t, nil
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return today.AddDate(0, 0, 1), nil
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func nextRandomShiftIndex(employeeName string, cycleIndex int, prev int) int {
	if len(ShiftOrder) == 0 {
		return 0
	}
	key := fmt.Sprintf("%s:%d", employeeName, cycleIndex)
	next := int(hash32(key) % uint32(len(ShiftOrder)))
	if next == prev {
		next = (next + 1) % len(ShiftOrder)
	}
	return next
}

func advanceState(s EmployeeState, days int, employeeName string, randomizeShifts bool) EmployeeState {
	if days <= 0 {
		return s
	}
	for i := 0; i < days; i++ {
		s.CycleDay++
		if s.CycleDay >= 7 {
			s.CycleDay = 0
			s.CycleIndex++
			if randomizeShifts {
				s.ShiftIndex = nextRandomShiftIndex(employeeName, s.CycleIndex, s.ShiftIndex)
			} else {
				s.ShiftIndex = (s.ShiftIndex + 1) % len(ShiftOrder)
			}
		}
	}
	return s
}

func isLeaveOn(date time.Time, leaves []LeavePeriod) bool {
	if len(leaves) == 0 {
		return false
	}
	d := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
	for _, lp := range leaves {
		from, err1 := time.Parse(DateLayoutYMD, strings.TrimSpace(lp.From))
		to, err2 := time.Parse(DateLayoutYMD, strings.TrimSpace(lp.To))
		if err1 != nil || err2 != nil {
			continue
		}
		from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, d.Location())
		to = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, d.Location())
		if d.Equal(from) || d.Equal(to) || (d.After(from) && d.Before(to)) {
			return true
		}
		if d.Equal(from) {
			return true
		}
		if !d.Before(from) && !d.After(to) {
			return true
		}
	}
	return false
}

func buildEmployeeSchedule(totalDays int, startDate time.Time, employeeName string, startState EmployeeState, leaves []LeavePeriod, randomizeShifts bool) ([]string, EmployeeState) {
	sched := make([]string, 0, totalDays)
	state := startState
	for day := 0; day < totalDays; day++ {
		curDate := startDate.AddDate(0, 0, day)
		if isLeaveOn(curDate, leaves) {
			sched = append(sched, ShiftLeave)
			state = advanceState(state, 1, employeeName, randomizeShifts)
			continue
		}
		if state.CycleDay < 5 {
			code := ShiftOrder[state.ShiftIndex%len(ShiftOrder)]
			sched = append(sched, code)
		} else {
			sched = append(sched, ShiftOff)
		}
		state = advanceState(state, 1, employeeName, randomizeShifts)
	}
	return sched, state
}

func filterActiveEmployees(employees []EmployeeConfig) []EmployeeConfig {
	out := make([]EmployeeConfig, 0, len(employees))
	for _, e := range employees {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		if !e.Active {
			continue
		}
		e.Name = name
		out = append(out, e)
	}
	return out
}

func buildDefaultEmployees(count int) []EmployeeConfig {
	out := make([]EmployeeConfig, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, EmployeeConfig{Name: fmt.Sprintf("Employee %02d", i+1), Active: true})
	}
	return out
}

func seedForName(name string) EmployeeState {
	// Simple deterministic seed so adding/removing employees doesn't reshuffle others.
	h := 0
	for _, r := range name {
		h = (h*31 + int(r)) & 0x7fffffff
	}
	shiftIndex := h % len(ShiftOrder)
	cycleDay := (h / 7) % 7
	return EmployeeState{ShiftIndex: shiftIndex, CycleDay: cycleDay, CycleIndex: 0}
}

func buildSeedStates(employees []EmployeeConfig) map[string]EmployeeState {
	out := make(map[string]EmployeeState, len(employees))
	for _, e := range employees {
		out[e.Name] = seedForName(e.Name)
	}
	return out
}

func continuityToState(c *EmployeeContinuity) (EmployeeState, bool) {
	if c == nil {
		return EmployeeState{}, false
	}
	shiftIdx := -1
	for i, s := range ShiftOrder {
		if s == strings.TrimSpace(c.Shift) {
			shiftIdx = i
			break
		}
	}
	if shiftIdx < 0 {
		return EmployeeState{}, false
	}
	cd := c.CycleDay
	if cd < 0 || cd > 6 {
		return EmployeeState{}, false
	}
	ci := c.CycleIndex
	if ci < 0 {
		ci = 0
	}
	return EmployeeState{ShiftIndex: shiftIdx, CycleDay: cd, CycleIndex: ci}, true
}

func alignStatesForStart(reset bool, roster *RosterFile, employees []EmployeeConfig, seeds map[string]EmployeeState, start time.Time, randomizeShifts bool) ([]EmployeeState, error) {
	if reset || roster == nil {
		out := make([]EmployeeState, 0, len(employees))
		for _, e := range employees {
			out = append(out, seeds[e.Name])
		}
		return out, nil
	}

	// If no continuity date, just use seeds (no alignment).
	if strings.TrimSpace(roster.NextRosterStartDate) == "" {
		out := make([]EmployeeState, 0, len(employees))
		for _, e := range employees {
			st := seeds[e.Name]
			if s2, ok := continuityToState(e.Continuity); ok {
				st = s2
			}
			out = append(out, st)
		}
		return out, nil
	}

	base, err := time.Parse(DateLayoutYMD, roster.NextRosterStartDate)
	if err != nil {
		return nil, fmt.Errorf("invalid nextRosterStartDate in config file: %w", err)
	}
	base = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, start.Location())

	if start.Before(base) {
		// Starting earlier than state would require rewinding; keep behavior explicit.
		return nil, fmt.Errorf("start date %s is before saved nextRosterStartDate %s; use --reset-state to start fresh", start.Format(DateLayoutYMD), base.Format(DateLayoutYMD))
	}

	deltaDays := int(start.Sub(base).Hours() / 24)
	out := make([]EmployeeState, 0, len(employees))
	for _, e := range employees {
		seed := seeds[e.Name]
		startState := seed
		if s2, ok := continuityToState(e.Continuity); ok {
			startState = s2
		}
		out = append(out, advanceState(startState, deltaDays, e.Name, randomizeShifts))
	}
	return out, nil
}

func loadRosterFile(path string) (*RosterFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c RosterFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}
	return &c, nil
}

func saveRosterFile(path string, c *RosterFile) error {
	if c == nil {
		return errors.New("nil roster config")
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func loadLegacyRosterState(path string) (*legacyRosterState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s legacyRosterState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("failed to parse legacy state file %s: %w", path, err)
	}
	if s.Employees == nil {
		s.Employees = map[string]EmployeeState{}
	}
	return &s, nil
}

func updateEmployeeContinuity(roster *RosterFile, employeeName string, st EmployeeState) {
	if roster == nil {
		return
	}
	for i := range roster.Employees {
		if strings.TrimSpace(roster.Employees[i].Name) != employeeName {
			continue
		}
		roster.Employees[i].Continuity = &EmployeeContinuity{
			Shift:      ShiftOrder[st.ShiftIndex%len(ShiftOrder)],
			CycleDay:   st.CycleDay,
			CycleIndex: st.CycleIndex,
		}
		return
	}
}

func ensureMinDailyCoverage(schedules [][]string, minPerShift int) error {
	if len(schedules) == 0 {
		return errors.New("no schedules")
	}
	totalDays := len(schedules[0])
	for day := 0; day < totalDays; day++ {
		counts := map[string]int{ShiftOrder[0]: 0, ShiftOrder[1]: 0, ShiftOrder[2]: 0}
		for _, s := range schedules {
			code := s[day]
			if _, ok := counts[code]; ok {
				counts[code]++
			}
		}
		if counts[ShiftOrder[0]] < minPerShift || counts[ShiftOrder[1]] < minPerShift || counts[ShiftOrder[2]] < minPerShift {
			return fmt.Errorf("coverage check failed on day_index=%d: %+v", day, counts)
		}
	}
	return nil
}

// ---------- Excel helpers ----------

func colName(col int) string {
	name, _ := excelize.ColumnNumberToName(col)
	return name
}

func cellAddr(col, row int) string {
	return fmt.Sprintf("%s%d", colName(col), row)
}

func newHeaderStyle(f *excelize.File) (int, error) {
	return f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#1F4E79"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Style: 1, Color: "000000"},
			{Type: "right", Style: 1, Color: "000000"},
			{Type: "top", Style: 1, Color: "000000"},
			{Type: "bottom", Style: 1, Color: "000000"},
		},
	})
}

func newWeekHeaderStyle(f *excelize.File) (int, error) {
	return f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#2E75B6"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Style: 1, Color: "000000"},
			{Type: "right", Style: 1, Color: "000000"},
			{Type: "top", Style: 1, Color: "000000"},
			{Type: "bottom", Style: 1, Color: "000000"},
		},
	})
}

func newCellStyle(f *excelize.File, bgHex string, whiteText bool, bold bool) (int, error) {
	font := &excelize.Font{Bold: bold, Size: 10}
	if whiteText {
		font.Color = "FFFFFF"
	}
	return f.NewStyle(&excelize.Style{
		Font:      font,
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#" + bgHex}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Style: 1, Color: "000000"},
			{Type: "right", Style: 1, Color: "000000"},
			{Type: "top", Style: 1, Color: "000000"},
			{Type: "bottom", Style: 1, Color: "000000"},
		},
	})
}

func newPlainBorderStyle(f *excelize.File, bold bool) (int, error) {
	return f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: bold, Size: 10},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Style: 1, Color: "000000"},
			{Type: "right", Style: 1, Color: "000000"},
			{Type: "top", Style: 1, Color: "000000"},
			{Type: "bottom", Style: 1, Color: "000000"},
		},
	})
}

func createRosterSheet(f *excelize.File, start time.Time, totalDays int, employees []EmployeeConfig, schedules [][]string) error {
	sheet := SheetRoster
	idx, err := f.NewSheet(sheet)
	if err != nil {
		return err
	}
	f.SetActiveSheet(idx)

	headerStyle, err := newHeaderStyle(f)
	if err != nil {
		return err
	}
	weekHeaderStyle, err := newWeekHeaderStyle(f)
	if err != nil {
		return err
	}
	nameStyle, err := newPlainBorderStyle(f, true)
	if err != nil {
		return err
	}
	offStyle, err := newCellStyle(f, Shifts[ShiftOff].Color, false, false)
	if err != nil {
		return err
	}
	leaveStyle, err := newCellStyle(f, Shifts[ShiftLeave].Color, false, true)
	if err != nil {
		return err
	}
	d1Style, err := newCellStyle(f, Shifts[ShiftD1].Color, false, true)
	if err != nil {
		return err
	}
	d3Style, err := newCellStyle(f, Shifts[ShiftD3].Color, false, true)
	if err != nil {
		return err
	}
	d5Style, err := newCellStyle(f, Shifts[ShiftD5].Color, true, true)
	if err != nil {
		return err
	}

	lastCol := 1 + totalDays
	lastColLetter := colName(lastCol)

	// Title rows
	_ = f.MergeCell(sheet, "A1", lastColLetter+"1")
	_ = f.SetCellValue(sheet, "A1", "24/7 EMPLOYEE SHIFT ROSTER")
	_ = f.SetCellStyle(sheet, "A1", "A1", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 16, Color: "1F4E79"}, Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"}}))

	end := start.AddDate(0, 0, totalDays-1)
	_ = f.MergeCell(sheet, "A2", lastColLetter+"2")
	_ = f.SetCellValue(sheet, "A2", fmt.Sprintf("Schedule Period: %s - %s | Shifts: Morning (6AM-2PM), Afternoon (2PM-10PM), Night (10PM-6AM)", start.Format("02 Jan 2006"), end.Format("02 Jan 2006")))
	_ = f.SetCellStyle(sheet, "A2", "A2", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Italic: true, Size: 10}, Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"}}))

	_ = f.MergeCell(sheet, "A3", lastColLetter+"3")
	_ = f.SetCellValue(sheet, "A3", "Legend: D1 = Morning (Green) | D3 = Afternoon (Yellow) | D5 = Night (Blue) | OFF = Day Off (Gray) | L = Leave | Shift changes after each off block")
	_ = f.SetCellStyle(sheet, "A3", "A3", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Size: 9}, Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"}}))

	// Row 4 week headers
	_ = f.SetCellValue(sheet, "A4", "Employee")
	_ = f.SetCellStyle(sheet, "A4", "A4", headerStyle)

	weeks := int(math.Ceil(float64(totalDays) / 7.0))
	for week := 0; week < weeks; week++ {
		startCol := 2 + week*7
		endCol := startCol + 6
		maxEnd := 1 + totalDays
		if endCol > maxEnd {
			endCol = maxEnd
		}
		from := cellAddr(startCol, 4)
		to := cellAddr(endCol, 4)
		_ = f.MergeCell(sheet, from, to)
		ws := start.AddDate(0, 0, week*7)
		we := ws.AddDate(0, 0, 6)
		if we.After(end) {
			we = end
		}
		_ = f.SetCellValue(sheet, from, fmt.Sprintf("Week %d: %s - %s", week+1, ws.Format("02 Jan"), we.Format("02 Jan")))
		_ = f.SetCellStyle(sheet, from, to, weekHeaderStyle)
	}

	// Row 5 date headers
	_ = f.SetCellValue(sheet, "A5", "Name")
	_ = f.SetCellStyle(sheet, "A5", "A5", headerStyle)
	for d := 0; d < totalDays; d++ {
		col := 2 + d
		cur := start.AddDate(0, 0, d)
		addr := cellAddr(col, 5)
		_ = f.SetCellValue(sheet, addr, fmt.Sprintf("%s\n%s", cur.Format("Mon"), cur.Format("02/01")))
		_ = f.SetCellStyle(sheet, addr, addr, headerStyle)
	}

	// Data rows
	for i, emp := range employees {
		row := 6 + i
		_ = f.SetCellValue(sheet, cellAddr(1, row), emp.Name)
		_ = f.SetCellStyle(sheet, cellAddr(1, row), cellAddr(1, row), nameStyle)
		for d := 0; d < totalDays; d++ {
			col := 2 + d
			addr := cellAddr(col, row)
			code := schedules[i][d]
			switch code {
			case ShiftOff:
				_ = f.SetCellValue(sheet, addr, ShiftOff)
				_ = f.SetCellStyle(sheet, addr, addr, offStyle)
			case ShiftLeave:
				_ = f.SetCellValue(sheet, addr, ShiftLeave)
				_ = f.SetCellStyle(sheet, addr, addr, leaveStyle)
			case ShiftD1:
				_ = f.SetCellValue(sheet, addr, ShiftD1)
				_ = f.SetCellStyle(sheet, addr, addr, d1Style)
			case ShiftD3:
				_ = f.SetCellValue(sheet, addr, ShiftD3)
				_ = f.SetCellStyle(sheet, addr, addr, d3Style)
			case ShiftD5:
				_ = f.SetCellValue(sheet, addr, ShiftD5)
				_ = f.SetCellStyle(sheet, addr, addr, d5Style)
			default:
				_ = f.SetCellValue(sheet, addr, code)
				_ = f.SetCellStyle(sheet, addr, addr, nameStyle)
			}
		}
	}

	_ = f.SetColWidth(sheet, "A", "A", 18)
	_ = f.SetColWidth(sheet, "B", lastColLetter, 6)

	// Freeze panes at B6
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       true,
		XSplit:      1,
		YSplit:      5,
		TopLeftCell: "B6",
		ActivePane:  "bottomRight",
	})

	return nil
}

func createShiftDetailsSheet(f *excelize.File, start time.Time, totalDays int, employeeCount int) error {
	sheet := SheetDetails
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	headerStyle, err := newHeaderStyle(f)
	if err != nil {
		return err
	}
	plainStyle, err := newPlainBorderStyle(f, false)
	if err != nil {
		return err
	}
	plainLeftStyle, err := f.NewStyle(&excelize.Style{
		Alignment: &excelize.Alignment{Horizontal: "left", Vertical: "center"},
		Border: []excelize.Border{
			{Type: "left", Style: 1, Color: "000000"},
			{Type: "right", Style: 1, Color: "000000"},
			{Type: "top", Style: 1, Color: "000000"},
			{Type: "bottom", Style: 1, Color: "000000"},
		},
	})
	if err != nil {
		return err
	}

	_ = f.MergeCell(sheet, "A1", "F1")
	_ = f.SetCellValue(sheet, "A1", "SHIFT DETAILS & COVERAGE SUMMARY")
	_ = f.SetCellStyle(sheet, "A1", "A1", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 16, Color: "1F4E79"}, Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"}}))

	_ = f.SetCellValue(sheet, "A3", "SHIFT SCHEDULE")
	_ = f.SetCellStyle(sheet, "A3", "A3", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}}))

	headers := []string{"Shift Name", "Code", "Start Time", "End Time", "Duration", "Color Code"}
	for c, h := range headers {
		addr := cellAddr(1+c, 4)
		_ = f.SetCellValue(sheet, addr, h)
		_ = f.SetCellStyle(sheet, addr, addr, headerStyle)
	}

	rows := [][]string{
		{"Morning", ShiftD1, "6:00 AM", "2:00 PM", "8 hours", "Green"},
		{"Afternoon", ShiftD3, "2:00 PM", "10:00 PM", "8 hours", "Yellow"},
		{"Night", ShiftD5, "10:00 PM", "6:00 AM", "8 hours", "Blue"},
		{"Leave", ShiftLeave, "-", "-", "-", "Light Orange"},
	}

	for i, r := range rows {
		row := 5 + i
		code := r[1]
		style, err := newCellStyle(f, Shifts[code].Color, code == ShiftD5, false)
		if err != nil {
			return err
		}
		for c, v := range r {
			addr := cellAddr(1+c, row)
			_ = f.SetCellValue(sheet, addr, v)
			_ = f.SetCellStyle(sheet, addr, addr, style)
		}
	}

	_ = f.SetCellValue(sheet, "A10", "WORK PATTERN RULES")
	_ = f.SetCellStyle(sheet, "A10", "A10", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}}))

	for c, h := range []string{"Rule", "Description"} {
		addr := cellAddr(1+c, 11)
		_ = f.SetCellValue(sheet, addr, h)
		_ = f.SetCellStyle(sheet, addr, addr, headerStyle)
	}

	patterns := [][]string{
		{"Work Days", "5 consecutive days per cycle"},
		{"Days Off", "2 consecutive days per cycle (e.g., Sat-Sun, Mon-Tue)"},
		{"Shift Consistency", "Same shift for entire 5-day working block"},
		{"Shift Rotation", "Shift changes after each off block (D1 → D3 → D5 → D1...)"},
		{"Rotation Length", "Configurable number of days"},
	}
	for i, r := range patterns {
		row := 12 + i
		_ = f.SetCellValue(sheet, cellAddr(1, row), r[0])
		_ = f.SetCellValue(sheet, cellAddr(2, row), r[1])
		_ = f.SetCellStyle(sheet, cellAddr(1, row), cellAddr(1, row), plainStyle)
		_ = f.SetCellStyle(sheet, cellAddr(2, row), cellAddr(2, row), plainLeftStyle)
	}

	_ = f.SetCellValue(sheet, "A21", "COVERAGE SUMMARY")
	_ = f.SetCellStyle(sheet, "A21", "A21", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}}))

	for c, h := range []string{"Metric", "Value"} {
		addr := cellAddr(1+c, 22)
		_ = f.SetCellValue(sheet, addr, h)
		_ = f.SetCellStyle(sheet, addr, addr, headerStyle)
	}

	end := start.AddDate(0, 0, totalDays-1)
	summary := [][]string{
		{"Total Employees", fmt.Sprintf("%d", employeeCount)},
		{"Shifts per Day", "3"},
		{"Schedule Duration", fmt.Sprintf("%d days", totalDays)},
		{"Schedule Start Date", start.Format("02 January 2006")},
		{"Schedule End Date", end.Format("02 January 2006")},
		{"Min Required Coverage", "1 employee per shift per day"},
		{"Note", "With many employees, coverage is > minimum"},
	}
	for i, r := range summary {
		row := 23 + i
		_ = f.SetCellValue(sheet, cellAddr(1, row), r[0])
		_ = f.SetCellValue(sheet, cellAddr(2, row), r[1])
		_ = f.SetCellStyle(sheet, cellAddr(1, row), cellAddr(1, row), plainStyle)
		_ = f.SetCellStyle(sheet, cellAddr(2, row), cellAddr(2, row), plainStyle)
	}

	_ = f.SetColWidth(sheet, "A", "A", 25)
	_ = f.SetColWidth(sheet, "B", "B", 55)
	_ = f.SetColWidth(sheet, "C", "F", 15)
	return nil
}

func createEmployeeMasterSheet(f *excelize.File, totalDays int, employees []EmployeeConfig, schedules [][]string) error {
	sheet := SheetEmployees
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}

	headerStyle, err := newHeaderStyle(f)
	if err != nil {
		return err
	}
	plainStyle, err := newPlainBorderStyle(f, false)
	if err != nil {
		return err
	}
	activeStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Color: "006100"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#C6EFCE"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
		Border: []excelize.Border{
			{Type: "left", Style: 1, Color: "000000"},
			{Type: "right", Style: 1, Color: "000000"},
			{Type: "top", Style: 1, Color: "000000"},
			{Type: "bottom", Style: 1, Color: "000000"},
		},
	})
	if err != nil {
		return err
	}

	_ = f.MergeCell(sheet, "A1", "E1")
	_ = f.SetCellValue(sheet, "A1", "EMPLOYEE MASTER DATA")
	_ = f.SetCellStyle(sheet, "A1", "A1", mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 16, Color: "1F4E79"}, Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"}}))

	headers := []string{"Full Name", "Shift Preference", "Status", "Email", "Phone"}
	for c, h := range headers {
		addr := cellAddr(1+c, 3)
		_ = f.SetCellValue(sheet, addr, h)
		_ = f.SetCellStyle(sheet, addr, addr, headerStyle)
	}

	// Sample data for first 5 employees
	sample := [][]string{
		{"John Smith", "Morning", "Active", "john.smith@company.com", "+1-555-0101"},
		{"Sarah Johnson", "Afternoon", "Active", "sarah.johnson@company.com", "+1-555-0102"},
		{"Michael Brown", "Night", "Active", "michael.brown@company.com", "+1-555-0103"},
		{"Emily Davis", "Rotating", "Active", "emily.davis@company.com", "+1-555-0104"},
		{"David Wilson", "Rotating", "Active", "david.wilson@company.com", "+1-555-0105"},
	}

	for i, emp := range employees {
		row := 4 + i
		var vals []string
		if i < len(sample) {
			vals = sample[i]
		} else {
			email := strings.ToLower(strings.ReplaceAll(emp.Name, " ", ".")) + "@company.com"
			phone := fmt.Sprintf("+1-555-%04d", 1000+i)
			vals = []string{emp.Name, "Rotating", "Active", email, phone}
		}

		for c, v := range vals {
			addr := cellAddr(1+c, row)
			_ = f.SetCellValue(sheet, addr, v)
			if c == 2 && v == "Active" {
				_ = f.SetCellStyle(sheet, addr, addr, activeStyle)
			} else {
				_ = f.SetCellStyle(sheet, addr, addr, plainStyle)
			}
		}
	}

	statsTitleRow := 6 + len(employees)
	_ = f.SetCellValue(sheet, cellAddr(1, statsTitleRow), "EMPLOYEE STATISTICS")
	_ = f.SetCellStyle(sheet, cellAddr(1, statsTitleRow), cellAddr(1, statsTitleRow), mustNewStyle(f, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}}))

	statsHeaders := []string{"Employee Name", fmt.Sprintf("Total Work Days (%dd)", totalDays), "Total Hours (8h)", "Shifts Worked"}
	for c, h := range statsHeaders {
		addr := cellAddr(1+c, statsTitleRow+1)
		_ = f.SetCellValue(sheet, addr, h)
		_ = f.SetCellStyle(sheet, addr, addr, headerStyle)
	}

	for i, emp := range employees {
		s := schedules[i]
		workDays := 0
		seen := map[string]bool{}
		for _, code := range s {
			if code != ShiftOff && code != ShiftLeave {
				workDays++
				seen[code] = true
			}
		}
		hours := workDays * 8
		shifts := make([]string, 0, 3)
		for _, code := range []string{ShiftD1, ShiftD3, ShiftD5} {
			if seen[code] {
				shifts = append(shifts, code)
			}
		}
		shiftsStr := strings.Join(shifts, ", ")

		row := statsTitleRow + 2 + i
		vals := []any{emp.Name, workDays, hours, shiftsStr}
		for c, v := range vals {
			addr := cellAddr(1+c, row)
			_ = f.SetCellValue(sheet, addr, v)
			_ = f.SetCellStyle(sheet, addr, addr, plainStyle)
		}
	}

	_ = f.SetColWidth(sheet, "A", "A", 22)
	_ = f.SetColWidth(sheet, "B", "B", 18)
	_ = f.SetColWidth(sheet, "C", "C", 18)
	_ = f.SetColWidth(sheet, "D", "D", 18)
	_ = f.SetColWidth(sheet, "E", "E", 28)

	return nil
}
