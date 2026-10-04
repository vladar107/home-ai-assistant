package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolMoviesNext     = "movies_next"
	toolMoviesSetScore = "movies_set_score"
	toolMoviesSchedule = "movies_schedule"

	movieFilmProperty = "Фильм"
	movieDateLayout   = "02.01.2006"
	movieDateInput    = "2.1.2006"
	movieWeek         = 7
	movieRowPages     = 10
)

const movieGuidance = " Movie nights live in a Notion table whose title column holds the evening's date as DD.MM.YYYY and whose Фильм column holds the film. " +
	"For that table use movies_next to answer which film is next, never by reading the rows yourself. " +
	"Each single-letter column of numbers holds the scores of the household member whose name starts with that letter, a Latin and a Cyrillic form of a letter counting as the same, so the user's own scores are in the column of their initial."

const movieWriteGuidance = " For the movie table, movies_set_score records the user's own score in their own column and movies_schedule gives films their evening dates; " +
	"use them instead of notion_update_page, because they pick the column and compute the dates."

var movieReadDefinitions = []tools.Definition{
	{
		Name:        toolMoviesNext,
		Description: "Find the next movie night in the movie table: the evening with the earliest date on or after today, its film, and who picked it.",
		Risk:        tools.RiskRead,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result."}},` +
			`"required":["data_source_id"],"additionalProperties":false}`),
	},
}

var movieWriteDefinitions = []tools.Definition{
	{
		Name:        toolMoviesSetScore,
		Description: "Propose recording the current user's score for a film in their own score column of the movie table. The user must confirm before anything changes.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result."},` +
			`"film":{"type":"string","description":"The film title as the Фильм column spells it."},` +
			`"score":{"type":"number","minimum":0,"maximum":10,"description":"The score from 0 to 10, in steps of 0.5."}},` +
			`"required":["data_source_id","film","score"],"additionalProperties":false}`),
	},
	{
		Name: toolMoviesSchedule,
		Description: "Propose evening dates in the movie table. Without film, every film without a date gets one, in table order, each a week after the latest date in the table. " +
			"With film, only that film is scheduled: on date when given, otherwise a week after the latest other date. The user must confirm before anything changes.",
		Risk: tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result."},` +
			`"film":{"type":"string","description":"Schedule only this film, as the Фильм column spells it."},` +
			`"date":{"type":"string","description":"A specific evening date as YYYY-MM-DD; requires film."}},` +
			`"required":["data_source_id"],"additionalProperties":false}`),
	},
}

func isMovieTool(name string) bool {
	return name == toolMoviesNext || name == toolMoviesSetScore || name == toolMoviesSchedule
}

type movieTable struct {
	titleProperty string
	scoreColumns  []string
	pickerColumns []string
	rows          []movieRow
	truncated     bool
}

type movieRow struct {
	page   Object
	date   time.Time
	dated  bool
	film   string
	values map[string]string
}

func (s *Service) movies(ctx context.Context, principalID, token string, call tools.Call) (string, error) {
	var arguments struct {
		DataSourceID string   `json:"data_source_id"`
		Film         string   `json:"film"`
		Score        *float64 `json:"score"`
		Date         string   `json:"date"`
	}
	if err := decodeArguments(call.Arguments, &arguments); err != nil {
		return "", err
	}
	if strings.TrimSpace(arguments.DataSourceID) == "" {
		return "", fmt.Errorf("data_source_id is required")
	}
	film := strings.TrimSpace(arguments.Film)
	switch call.Name {
	case toolMoviesSetScore:
		if film == "" {
			return "", fmt.Errorf("film is required")
		}
		if arguments.Score == nil {
			return "", fmt.Errorf("score is required")
		}
		if score := *arguments.Score; score < 0 || score > 10 || math.Mod(score*2, 1) != 0 {
			return "", fmt.Errorf("score must be between 0 and 10 in steps of 0.5")
		}
	case toolMoviesSchedule:
		if arguments.Date != "" && film == "" {
			return "", fmt.Errorf("date requires film")
		}
	}
	var date time.Time
	if arguments.Date != "" {
		parsed, err := time.ParseInLocation(time.DateOnly, arguments.Date, s.location)
		if err != nil {
			return "", fmt.Errorf("date must be YYYY-MM-DD")
		}
		date = parsed
	}

	table, err := s.movieTable(ctx, token, arguments.DataSourceID)
	if err != nil {
		return "", err
	}
	switch call.Name {
	case toolMoviesNext:
		return s.nextMovie(table), nil
	case toolMoviesSetScore:
		return s.proposeScore(ctx, principalID, token, table, film, *arguments.Score)
	default:
		return s.proposeSchedule(ctx, principalID, token, table, film, date)
	}
}

// movieTable reads every row of the movie table in creation order, because the API exposes
// neither a view's sort nor its manual order and new films are added at the bottom.
func (s *Service) movieTable(ctx context.Context, token, id string) (movieTable, error) {
	dataSource, err := s.client.DataSource(ctx, token, id)
	if err != nil {
		var notionErr *Error
		if !errorAs(err, &notionErr) || (notionErr.Class != ErrorNotShared && notionErr.Class != ErrorInvalid) {
			return movieTable{}, err
		}
		database, databaseErr := s.client.Database(ctx, token, id)
		if databaseErr != nil || len(database.DataSources) != 1 {
			return movieTable{}, err
		}
		if dataSource, err = s.client.DataSource(ctx, token, database.DataSources[0].ID); err != nil {
			return movieTable{}, err
		}
	}
	table := movieTable{}
	var letterColumns []string
	for name, raw := range dataSource.Properties {
		var property struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &property)
		switch {
		case property.Type == "title":
			table.titleProperty = name
		case property.Type == "rich_text" && name != movieFilmProperty && utf8.RuneCountInString(strings.TrimSpace(name)) == 1:
			letterColumns = append(letterColumns, name)
		}
	}
	if _, hasFilm := dataSource.Properties[movieFilmProperty]; table.titleProperty == "" || !hasFilm {
		return movieTable{}, fmt.Errorf("this is not the movie table: it needs a title column holding dates as DD.MM.YYYY and a text column named %s", movieFilmProperty)
	}
	slices.Sort(letterColumns)

	cursor := ""
	for range movieRowPages {
		list, err := s.client.QueryDataSource(ctx, token, dataSource.ID, QueryRequest{
			Sorts: json.RawMessage(`[{"timestamp":"created_time","direction":"ascending"}]`), PageSize: MaximumPageSize, Cursor: cursor,
		})
		if err != nil {
			return movieTable{}, err
		}
		for _, page := range list.Results {
			table.rows = append(table.rows, s.movieRow(page, table.titleProperty))
		}
		if !list.HasMore || list.NextCursor == "" {
			cursor = ""
			break
		}
		cursor = list.NextCursor
	}
	table.truncated = cursor != ""

	for _, column := range letterColumns {
		if table.numericColumn(column) {
			table.scoreColumns = append(table.scoreColumns, column)
		} else {
			table.pickerColumns = append(table.pickerColumns, column)
		}
	}
	return table, nil
}

func (s *Service) movieRow(page Object, titleProperty string) movieRow {
	row := movieRow{page: page, values: map[string]string{}}
	for name, raw := range page.Properties {
		var property struct {
			Type     string     `json:"type"`
			Title    []RichText `json:"title"`
			RichText []RichText `json:"rich_text"`
		}
		if json.Unmarshal(raw, &property) != nil {
			continue
		}
		switch property.Type {
		case "title":
			row.values[name] = strings.TrimSpace(plainText(property.Title))
		case "rich_text":
			row.values[name] = strings.TrimSpace(plainText(property.RichText))
		}
	}
	row.film = row.values[movieFilmProperty]
	if date, err := time.ParseInLocation(movieDateInput, row.values[titleProperty], s.location); err == nil {
		row.date, row.dated = date, true
	}
	return row
}

// numericColumn tells a score column from the picker column that shares its letter: every filled
// score is a number, while the picker holds initials.
func (t movieTable) numericColumn(column string) bool {
	for _, row := range t.rows {
		value := strings.ReplaceAll(row.values[column], ",", ".")
		if value == "" {
			continue
		}
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return false
		}
	}
	return true
}

func (s *Service) nextMovie(table movieTable) string {
	now := s.now().In(s.location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.location)
	var next, latest *movieRow
	for i := range table.rows {
		row := &table.rows[i]
		if !row.dated {
			continue
		}
		if !row.date.Before(today) && (next == nil || row.date.Before(next.date)) {
			next = row
		}
		if row.date.Before(today) && (latest == nil || row.date.After(latest.date)) {
			latest = row
		}
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Today is %s.\n", movieDay(today))
	switch {
	case next == nil && latest == nil:
		builder.WriteString("No movie night has a date yet.")
	case next == nil:
		fmt.Fprintf(&builder, "No upcoming movie night is scheduled. The latest was %s on %s.", movieFilm(*latest), movieDay(latest.date))
	case next.film == "":
		fmt.Fprintf(&builder, "The next movie night is %s, but no film is set for it yet.", movieDay(next.date))
	default:
		fmt.Fprintf(&builder, "Next movie: %s on %s", next.film, movieDay(next.date))
		if picker := table.picker(*next); picker != "" {
			fmt.Fprintf(&builder, ", picked by %s", picker)
		}
		fmt.Fprintf(&builder, ". Row: %s", next.page.URL)
	}
	if waiting := table.undated(); len(waiting) > 0 {
		names := make([]string, 0, len(waiting))
		for _, row := range waiting {
			names = append(names, row.film)
		}
		fmt.Fprintf(&builder, "\nFilms without a date yet, in table order: %s.", strings.Join(names, ", "))
	}
	if table.truncated {
		builder.WriteString("\nThe table has more rows than were read, so later evenings may be missing.")
	}
	return builder.String()
}

func (t movieTable) picker(row movieRow) string {
	if len(t.pickerColumns) != 1 {
		return ""
	}
	return row.values[t.pickerColumns[0]]
}

func (t movieTable) undated() []movieRow {
	var rows []movieRow
	for _, row := range t.rows {
		if !row.dated && row.film != "" {
			rows = append(rows, row)
		}
	}
	return rows
}

// latestDate is the anchor new evenings continue from: the latest date anywhere in the table,
// whatever the row order, leaving out the row being rescheduled.
func (t movieTable) latestDate(except string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, row := range t.rows {
		if row.dated && row.page.ID != except && (!found || row.date.After(latest)) {
			latest, found = row.date, true
		}
	}
	return latest, found
}

func (t movieTable) matching(film string) []movieRow {
	var rows []movieRow
	for _, row := range t.rows {
		if strings.EqualFold(row.film, film) {
			rows = append(rows, row)
		}
	}
	return rows
}

func (t movieTable) noFilm(film string) error {
	var similar []string
	query := strings.ToLower(film)
	for _, row := range t.rows {
		title := strings.ToLower(row.film)
		if row.film != "" && (strings.Contains(title, query) || strings.Contains(query, title)) && len(similar) < 5 {
			similar = append(similar, row.film)
		}
	}
	if len(similar) == 0 {
		return fmt.Errorf("the movie table has no film %q", film)
	}
	return fmt.Errorf("the movie table has no film %q; similar titles are %s", film, strings.Join(similar, " | "))
}

func (s *Service) proposeScore(ctx context.Context, principalID, token string, table movieTable, film string, score float64) (string, error) {
	column, err := table.scoreColumn(s.displayNames[principalID])
	if err != nil {
		return "", err
	}
	rows := table.matching(film)
	if len(rows) == 0 {
		return "", table.noFilm(film)
	}
	row := rows[len(rows)-1]
	for _, candidate := range rows {
		if candidate.dated && (!row.dated || candidate.date.After(row.date)) {
			row = candidate
		}
	}
	properties := map[string]any{column: richTextValue(strconv.FormatFloat(score, 'f', -1, 64))}
	if err := s.proposeRowUpdate(ctx, principalID, token, row.page.ID, properties); err != nil {
		return "", err
	}
	text := fmt.Sprintf("Score %s for %s goes to column %s.", strconv.FormatFloat(score, 'f', -1, 64), movieFilm(row), column)
	if len(rows) > 1 {
		text += fmt.Sprintf(" The table holds %d rows of this film; the most recent was chosen.", len(rows))
	}
	return text + " " + approval.ProposedText, nil
}

// scoreColumn is the score column whose single-letter header is the user's initial. Latin and
// Cyrillic look-alikes are the same letter, and a Latin initial also stands for its Cyrillic
// transliteration, so "Vlad" owns В and "Kim" owns К.
func (t movieTable) scoreColumn(displayName string) (string, error) {
	first, _ := utf8.DecodeRuneInString(strings.TrimSpace(displayName))
	if first == utf8.RuneError {
		return "", fmt.Errorf("the user has no display name to find their score column by")
	}
	initials := initialLetters(first)
	var owned []string
	for _, column := range t.scoreColumns {
		letter, _ := utf8.DecodeRuneInString(strings.TrimSpace(column))
		if slices.Contains(initials, foldLookalike(letter)) {
			owned = append(owned, column)
		}
	}
	if len(owned) == 1 {
		return owned[0], nil
	}
	reason := "no score column matches"
	if len(owned) > 1 {
		reason = "several score columns match"
	}
	return "", fmt.Errorf("%s the initial of %s; the score columns are %s, so nothing was proposed", reason, displayName, strings.Join(t.scoreColumns, " | "))
}

var cyrillicLookalikes = map[rune]rune{
	'A': 'А', 'B': 'В', 'C': 'С', 'E': 'Е', 'H': 'Н', 'K': 'К', 'M': 'М', 'O': 'О', 'P': 'Р', 'T': 'Т', 'X': 'Х', 'Y': 'У',
}

var cyrillicTransliterations = map[rune]rune{
	'A': 'А', 'B': 'Б', 'D': 'Д', 'E': 'Е', 'F': 'Ф', 'G': 'Г', 'I': 'И', 'K': 'К', 'L': 'Л', 'M': 'М',
	'N': 'Н', 'O': 'О', 'P': 'П', 'R': 'Р', 'S': 'С', 'T': 'Т', 'U': 'У', 'V': 'В', 'Z': 'З',
}

func foldLookalike(letter rune) rune {
	letter = unicode.ToUpper(letter)
	if cyrillic, found := cyrillicLookalikes[letter]; found {
		return cyrillic
	}
	return letter
}

func initialLetters(first rune) []rune {
	first = unicode.ToUpper(first)
	letters := []rune{foldLookalike(first)}
	if cyrillic, found := cyrillicTransliterations[first]; found && cyrillic != letters[0] {
		letters = append(letters, cyrillic)
	}
	return letters
}

func (s *Service) proposeSchedule(ctx context.Context, principalID, token string, table movieTable, film string, date time.Time) (string, error) {
	type evening struct {
		row  movieRow
		date time.Time
	}
	var evenings []evening
	if film == "" {
		anchor, found := table.latestDate("")
		waiting := table.undated()
		if len(waiting) == 0 {
			return "Every film in the movie table already has a date; nothing was proposed.", nil
		}
		if !found {
			return "", fmt.Errorf("no evening in the movie table has a date to continue from; schedule one film with a specific date first")
		}
		for i, row := range waiting {
			evenings = append(evenings, evening{row: row, date: anchor.AddDate(0, 0, movieWeek*(i+1))})
		}
	} else {
		rows := table.matching(film)
		if len(rows) == 0 {
			return "", table.noFilm(film)
		}
		row := rows[len(rows)-1]
		for _, candidate := range rows {
			if !candidate.dated {
				row = candidate
				break
			}
		}
		if date.IsZero() {
			anchor, found := table.latestDate(row.page.ID)
			if !found {
				return "", fmt.Errorf("no other evening in the movie table has a date to continue from; give a specific date")
			}
			date = anchor.AddDate(0, 0, movieWeek)
		}
		evenings = append(evenings, evening{row: row, date: date})
	}

	lines := make([]string, 0, len(evenings))
	for _, evening := range evenings {
		properties := map[string]any{table.titleProperty: map[string]any{"title": []any{map[string]any{"text": map[string]any{"content": evening.date.Format(movieDateLayout)}}}}}
		if err := s.proposeRowUpdate(ctx, principalID, token, evening.row.page.ID, properties); err != nil {
			if len(lines) > 0 {
				return "", fmt.Errorf("%w; already proposed: %s", err, strings.Join(lines, ", "))
			}
			return "", err
		}
		lines = append(lines, fmt.Sprintf("%s → %s", evening.row.film, movieDay(evening.date)))
	}
	return "Proposed dates: " + strings.Join(lines, ", ") + ". " + approval.ProposedText, nil
}

// proposeRowUpdate records the change as an ordinary notion_update_page proposal, so it is checked,
// confirmed, and executed exactly like a property change the model proposed itself.
func (s *Service) proposeRowUpdate(ctx context.Context, principalID, token, pageID string, properties map[string]any) error {
	encoded := make(map[string]json.RawMessage, len(properties))
	for name, value := range properties {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		encoded[name] = raw
	}
	arguments, err := json.Marshal(updateArguments{PageID: pageID, Properties: encoded})
	if err != nil {
		return err
	}
	return s.record(ctx, principalID, token, toolUpdatePage, arguments)
}

func richTextValue(text string) map[string]any {
	return map[string]any{"rich_text": []any{map[string]any{"text": map[string]any{"content": text}}}}
}

func movieDay(date time.Time) string {
	return date.Format(movieDateLayout) + " (" + date.Weekday().String() + ")"
}

func movieFilm(row movieRow) string {
	if row.film == "" {
		return "an evening without a film"
	}
	return row.film
}
