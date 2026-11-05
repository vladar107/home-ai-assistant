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
	toolMoviesAdd      = "movies_add"

	movieFilmProperty   = "Фильм"
	moviePickerProperty = "Кто"
	movieDateLayout     = "02.01.2006"
	movieDateInput      = "2.1.2006"
	movieWeek           = 7
	movieRowPages       = 10
	movieSearchPages    = 3
)

const movieGuidance = " Movie nights live in a Notion table whose title column holds the evening's date as DD.MM.YYYY and whose Фильм column holds the film. " +
	"For that table use movies_next to answer which film is next, never by reading the rows yourself; call it without data_source_id unless you already have the table's ID. " +
	"Each single-letter column of numbers holds the scores of the household member whose name starts with that letter, a Latin and a Cyrillic form of a letter counting as the same, so the user's own scores are in the column of their initial."

const movieWriteGuidance = " For the movie table, movies_add adds films to the user's next turns, movies_schedule gives films that have no date their evening dates, and movies_set_score records the user's own score in their own column. " +
	"They are the only way to add evenings or record scores, because they follow the two pickers' weekly turns and compute the dates and columns, so never create rows of the movie table with notion_create_page. " +
	"To change evenings already in the table, such as swapping two films, putting a different film on an evening, filling a reserved evening, or changing an evening's picker or date, update those rows with notion_update_page. " +
	"To swap films, change the Фильм column of both rows and keep their dates. " +
	"Call movies_add or movies_schedule at most once per turn: the evenings one call proposes are not in the table until the user confirms them, so a second call would plan the same evenings again."

var movieReadDefinitions = []tools.Definition{
	{
		Name:        toolMoviesNext,
		Description: "Find the next movie night in the movie table: the evening with the earliest date on or after today, its film, who picked it, and whose turn it is when it has no film yet.",
		Risk:        tools.RiskRead,
		Activity:    tools.ActivityNotion,
		Private:     true,
		ThirdParty:  true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result. May be omitted: the table is then found among the shared data sources."}},` +
			`"additionalProperties":false}`),
	},
}

var movieWriteDefinitions = []tools.Definition{
	{
		Name:        toolMoviesSetScore,
		Description: "Record the current user's score for a film in their own score column of the movie table. It is written at once when the user asked for it in a conversation that has read no outside content; otherwise it is proposed for the user to confirm.",
		Risk:        tools.RiskWrite,
		Activity:    tools.ActivityNotion,
		Private:     true,
		Timeout:     updateTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result."},` +
			`"film":{"type":"string","description":"The film title as the Фильм column spells it."},` +
			`"score":{"type":"number","minimum":0,"maximum":10,"description":"The score from 0 to 10, in steps of 0.5."}},` +
			`"required":["data_source_id","film","score"],"additionalProperties":false}`),
	},
	{
		Name: toolMoviesSchedule,
		Description: "Set evening dates in the movie table. Without film, every film without a date goes to its picker's next turn after the latest date, in table order, with an empty reserved evening added for each turn of the other picker in between. " +
			"With film, only that film is scheduled: on date when given, otherwise in its picker's next turn after the latest other date. " +
			"A call that adds evenings is proposed as one batch for the user to confirm; a call that only sets dates is written at once when the user asked for it in a conversation that has read no outside content, and is otherwise proposed.",
		Risk:     tools.RiskWrite,
		Activity: tools.ActivityNotion,
		Private:  true,
		Timeout:  updateTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result."},` +
			`"film":{"type":"string","description":"Schedule only this film, as the Фильм column spells it."},` +
			`"date":{"type":"string","description":"A specific evening date as YYYY-MM-DD; requires film."}},` +
			`"required":["data_source_id"],"additionalProperties":false}`),
	},
	{
		Name: toolMoviesAdd,
		Description: "Add films to the movie table in one call. The films fill the picker's empty reserved evenings in date order, then continue after the latest date week by week, with an empty reserved evening added for each turn of the other picker in between. " +
			"The picker is the current user unless picker names someone else. A call that adds evenings is proposed as one batch for the user to confirm, and nothing changes until then; a call that only fills reserved evenings is written at once when the user asked for it in a conversation that has read no outside content, and is otherwise proposed. At most 10 changes fit in one batch.",
		Risk:     tools.RiskWrite,
		Activity: tools.ActivityNotion,
		Private:  true,
		Timeout:  updateTimeout,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"data_source_id":{"type":"string","description":"The movie table's data source or database ID from an earlier result."},` +
			`"films":{"type":"array","minItems":1,"items":{"type":"string","minLength":1},"description":"The film titles, in the order they should be watched, each at most once."},` +
			`"picker":{"type":"string","description":"The initial of the household member who picks these films; defaults to the current user."}},` +
			`"required":["data_source_id","films"],"additionalProperties":false}`),
	},
}

func isMovieTool(name string) bool {
	return name == toolMoviesNext || name == toolMoviesSetScore || name == toolMoviesSchedule || name == toolMoviesAdd
}

type movieTable struct {
	dataSourceID   string
	titleProperty  string
	pickerProperty string
	scoreColumns   []string
	rows           []movieRow
	truncated      bool
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
		Films        []string `json:"films"`
		Picker       string   `json:"picker"`
	}
	if err := tools.DecodeArguments(call.Arguments, &arguments); err != nil {
		return "", err
	}
	if strings.TrimSpace(arguments.DataSourceID) == "" && call.Name != toolMoviesNext {
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
	case toolMoviesAdd:
		if err := validateMovieFilms(arguments.Films); err != nil {
			return "", err
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

	dataSourceID := arguments.DataSourceID
	discovered := strings.TrimSpace(dataSourceID) == ""
	if discovered {
		found, err := s.findMovieTable(ctx, token)
		if err != nil {
			return "", err
		}
		dataSourceID = found
	}
	table, err := s.movieTable(ctx, token, dataSourceID)
	if err != nil {
		return "", err
	}
	// The unread rows are the newest, which hold the latest date, the undated films, and the most
	// recent row of a film, so a write computed from the rows that were read could land wrongly.
	if table.truncated && call.Name != toolMoviesNext {
		return "", fmt.Errorf("the movie table has more rows than can be read (over %d), so its newest rows are unknown; nothing was proposed", movieRowPages*apiPageSizeLimit)
	}
	switch call.Name {
	case toolMoviesNext:
		if discovered {
			// The writes still need the ID, and this call is what spared the model a search for it.
			return s.nextMovie(table) + "\nThe movie table's data_source_id is " + dataSourceID + ".", nil
		}
		return s.nextMovie(table), nil
	case toolMoviesSetScore:
		return s.proposeScore(ctx, principalID, token, table, film, *arguments.Score)
	case toolMoviesAdd:
		return s.proposeAdd(ctx, principalID, token, table, arguments.Films, arguments.Picker)
	default:
		return s.proposeSchedule(ctx, principalID, token, table, film, date)
	}
}

func validateMovieFilms(films []string) error {
	if len(films) == 0 {
		return fmt.Errorf("films must name at least one film")
	}
	seen := map[string]bool{}
	for _, film := range films {
		title := strings.ToLower(strings.TrimSpace(film))
		if title == "" {
			return fmt.Errorf("films must not hold a blank title")
		}
		if seen[title] {
			return fmt.Errorf("films names %q more than once", strings.TrimSpace(film))
		}
		seen[title] = true
	}
	return nil
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
	table := movieTable{dataSourceID: dataSource.ID}
	var letterColumns []string
	for name, raw := range dataSource.Properties {
		var property struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &property)
		switch {
		case property.Type == "title":
			table.titleProperty = name
		case property.Type == "rich_text" && name == moviePickerProperty:
			table.pickerProperty = name
		case property.Type == "rich_text" && name != movieFilmProperty && utf8.RuneCountInString(strings.TrimSpace(name)) == 1:
			letterColumns = append(letterColumns, name)
		}
	}
	if table.titleProperty == "" || !hasFilmProperty(dataSource.Properties) {
		return movieTable{}, fmt.Errorf("this is not the movie table: it needs a title column holding dates as DD.MM.YYYY and a text column named %s", movieFilmProperty)
	}
	slices.Sort(letterColumns)

	cursor := ""
	for range movieRowPages {
		list, err := s.client.QueryDataSource(ctx, token, dataSource.ID, QueryRequest{
			Sorts: json.RawMessage(`[{"timestamp":"created_time","direction":"ascending"}]`), PageSize: apiPageSizeLimit, Cursor: cursor,
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

	var letterPickers []string
	for _, column := range letterColumns {
		if table.numericColumn(column) {
			table.scoreColumns = append(table.scoreColumns, column)
		} else {
			letterPickers = append(letterPickers, column)
		}
	}
	if table.pickerProperty == "" && len(letterPickers) == 1 {
		table.pickerProperty = letterPickers[0]
	}
	return table, nil
}

func hasFilmProperty(properties map[string]json.RawMessage) bool {
	_, found := properties[movieFilmProperty]
	return found
}

// findMovieTable picks the one shared data source with a Фильм column, so a call that has no ID
// does not depend on the model's search query.
func (s *Service) findMovieTable(ctx context.Context, token string) (string, error) {
	var candidates []Object
	cursor := ""
	for range movieSearchPages {
		list, err := s.client.Search(ctx, token, SearchRequest{ObjectType: "data_source", PageSize: apiPageSizeLimit, Cursor: cursor})
		if err != nil {
			return "", err
		}
		for _, result := range list.Results {
			if !result.InTrash && hasFilmProperty(result.Properties) {
				candidates = append(candidates, result)
			}
		}
		if !list.HasMore || list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no shared data source looks like the movie table: it needs a title column holding dates as DD.MM.YYYY and a text column named %s; the user must share it with their Notion connection", movieFilmProperty)
	case 1:
		return candidates[0].ID, nil
	}
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		names = append(names, fmt.Sprintf("%s (%s)", objectTitle(candidate), candidate.ID))
	}
	return "", fmt.Errorf("several shared data sources look like the movie table: %s; pass the right one as data_source_id", strings.Join(names, " | "))
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
	today := s.today()
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
		if picker := table.picker(*next); picker != "" {
			fmt.Fprintf(&builder, " It is %s's turn.", picker)
		}
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

func (s *Service) today() time.Time {
	now := s.now().In(s.location)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.location)
}

func (t movieTable) picker(row movieRow) string {
	if t.pickerProperty == "" {
		return ""
	}
	return row.values[t.pickerProperty]
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
	result, err := s.proposeRowUpdate(ctx, principalID, token, row.page.ID, properties)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("Score %s for %s goes to column %s.", strconv.FormatFloat(score, 'f', -1, 64), movieFilm(row), column)
	if len(rows) > 1 {
		text += fmt.Sprintf(" The table holds %d rows of this film; the most recent was chosen.", len(rows))
	}
	return text + " " + result, nil
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

// moviePicker is one of the two household members who take turns choosing the film. A picker is
// identified by the folded first letter of the value in the picker column, and spelling is the
// value as the table writes it.
type moviePicker struct {
	letter   rune
	spelling string
}

// movieTurns is the weekly rotation continuing from the latest evening: evening k is k weeks after
// it and belongs to the other picker when k is odd.
type movieTurns struct {
	latest  time.Time
	pickers [2]moviePicker
	first   int
}

func (t movieTurns) evening(k int) time.Time {
	return t.latest.AddDate(0, 0, movieWeek*k)
}

func pickerLetter(value string) rune {
	first, _ := utf8.DecodeRuneInString(strings.TrimSpace(value))
	if first == utf8.RuneError {
		return 0
	}
	return foldLookalike(first)
}

// turns reads the rotation from the latest dated row and the most recent dated row with a different
// picker, leaving out the row being rescheduled. New evenings are never before today.
func (t movieTable) turns(except string, today time.Time) (movieTurns, error) {
	if t.pickerProperty == "" {
		return movieTurns{}, fmt.Errorf("the movie table has no picker column (a text column named %s), so whose turn an evening is cannot be told; nothing was proposed. A single film can still be scheduled by giving film and date", moviePickerProperty)
	}
	var latest *movieRow
	for i := range t.rows {
		row := &t.rows[i]
		if row.dated && row.page.ID != except && (latest == nil || !row.date.Before(latest.date)) {
			latest = row
		}
	}
	if latest == nil {
		return movieTurns{}, fmt.Errorf("no evening in the movie table has a date to continue from; schedule one film with a specific date first")
	}
	first := pickerLetter(t.picker(*latest))
	if first == 0 {
		return movieTurns{}, fmt.Errorf("the latest evening, %s, has no picker in column %s, so the turns cannot continue from it; set it first, nothing was proposed", movieDay(latest.date), moviePickerProperty)
	}
	var other *movieRow
	for i := range t.rows {
		row := &t.rows[i]
		letter := pickerLetter(t.picker(*row))
		if row.dated && row.page.ID != except && letter != 0 && letter != first && (other == nil || !row.date.Before(other.date)) {
			other = row
		}
	}
	if other == nil {
		return movieTurns{}, fmt.Errorf("no dated evening in the movie table has a picker other than %s, so the turns cannot alternate; nothing was proposed", t.picker(*latest))
	}
	turns := movieTurns{
		latest:  latest.date,
		pickers: [2]moviePicker{{first, t.picker(*latest)}, {pickerLetter(t.picker(*other)), t.picker(*other)}},
		first:   1,
	}
	for turns.evening(turns.first).Before(today) {
		turns.first++
	}
	return turns, nil
}

func (t movieTurns) names() string {
	return t.pickers[0].spelling + " | " + t.pickers[1].spelling
}

func (t movieTurns) pickerOf(value string) (int, bool) {
	letter := pickerLetter(value)
	for index, picker := range t.pickers {
		if letter != 0 && letter == picker.letter {
			return index, true
		}
	}
	return 0, false
}

// pickerByInitial matches a person to a picker like a score column: the initial of their name, in
// its Latin or Cyrillic form, must be the picker's letter, and exactly one picker may match.
func (t movieTurns) pickerByInitial(name string) (int, error) {
	first, _ := utf8.DecodeRuneInString(strings.TrimSpace(name))
	if first == utf8.RuneError {
		return 0, fmt.Errorf("there is no name to tell which picker is meant; give picker (one of %s)", t.names())
	}
	initials := initialLetters(first)
	matched := -1
	for index, picker := range t.pickers {
		if !slices.Contains(initials, picker.letter) {
			continue
		}
		if matched >= 0 {
			return 0, fmt.Errorf("the initial of %s matches both pickers (%s); nothing was proposed", name, t.names())
		}
		matched = index
	}
	if matched < 0 {
		return 0, fmt.Errorf("the initial of %s matches neither picker (%s); give picker as one of them, nothing was proposed", name, t.names())
	}
	return matched, nil
}

// movieChange is one row write of a movies call, recorded as an ordinary notion_create_page or
// notion_update_page.
type movieChange struct {
	tool      string
	arguments json.RawMessage
	line      string
}

// moviePlan collects the changes of one call. filmEnds[i] is how many changes the first i+1 films
// need, so a batch that is too large can say how many films fit.
type moviePlan struct {
	table    movieTable
	turns    movieTurns
	next     int
	changes  []movieChange
	filmEnds []int
}

func newMoviePlan(table movieTable, turns movieTurns) *moviePlan {
	return &moviePlan{table: table, turns: turns, next: turns.first}
}

// place puts a film in the picker's next free turn, reserving each turn of the other picker that
// it passes. An existing row is given its date; without one a new row is created.
func (p *moviePlan) place(picker int, film string, existing *movieRow) error {
	for p.next%2 != picker {
		change, err := p.create(p.turns.evening(p.next), p.turns.pickers[p.next%2], "")
		if err != nil {
			return err
		}
		p.changes = append(p.changes, change)
		p.next++
	}
	date := p.turns.evening(p.next)
	p.next++
	var change movieChange
	var err error
	if existing != nil {
		change, err = movieDateChange(p.table, *existing, date)
	} else {
		change, err = p.create(date, p.turns.pickers[picker], film)
	}
	if err != nil {
		return err
	}
	p.changes = append(p.changes, change)
	p.filmEnds = append(p.filmEnds, len(p.changes))
	return nil
}

func (p *moviePlan) create(date time.Time, picker moviePicker, film string) (movieChange, error) {
	values := map[string]any{p.table.pickerProperty: richTextValue(picker.spelling)}
	if film != "" {
		values[movieFilmProperty] = richTextValue(film)
	}
	properties, err := encodeProperties(values)
	if err != nil {
		return movieChange{}, err
	}
	arguments, err := json.Marshal(createArguments{ParentType: "data_source", ParentID: p.table.dataSourceID, Title: date.Format(movieDateLayout), Properties: properties})
	if err != nil {
		return movieChange{}, err
	}
	line := fmt.Sprintf("%s reserved for %s", movieDay(date), picker.spelling)
	if film != "" {
		line = fmt.Sprintf("%s on %s (new evening, %s's turn)", film, movieDay(date), picker.spelling)
	}
	return movieChange{tool: toolCreatePage, arguments: arguments, line: line}, nil
}

func encodeProperties(values map[string]any) (map[string]json.RawMessage, error) {
	encoded := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		encoded[name] = raw
	}
	return encoded, nil
}

func movieUpdateChange(pageID string, values map[string]any, line string) (movieChange, error) {
	properties, err := encodeProperties(values)
	if err != nil {
		return movieChange{}, err
	}
	arguments, err := json.Marshal(updateArguments{PageID: pageID, Properties: properties})
	if err != nil {
		return movieChange{}, err
	}
	return movieChange{tool: toolUpdatePage, arguments: arguments, line: line}, nil
}

func movieDateChange(table movieTable, row movieRow, date time.Time) (movieChange, error) {
	properties := map[string]any{table.titleProperty: map[string]any{"title": []any{map[string]any{"text": map[string]any{"content": date.Format(movieDateLayout)}}}}}
	return movieUpdateChange(row.page.ID, properties, fmt.Sprintf("%s → %s", row.film, movieDay(date)))
}

// checkBundle refuses a batch the approval service could not hold in one confirmation before any
// of it is recorded, and says how many films fit.
func (p *moviePlan) checkBundle() error {
	if len(p.changes) <= approval.MaximumBundleItems {
		return nil
	}
	fit := 0
	for _, end := range p.filmEnds {
		if end <= approval.MaximumBundleItems {
			fit++
		}
	}
	return fmt.Errorf("these %d films need %d changes, but only %d can be confirmed together; only the first %d films fit, so nothing was proposed. Ask the user to confirm that part, then call again with the rest", len(p.filmEnds), len(p.changes), approval.MaximumBundleItems, fit)
}

func (s *Service) proposeAdd(ctx context.Context, principalID, token string, table movieTable, films []string, pickerName string) (string, error) {
	if waiting := table.undated(); len(waiting) > 0 {
		names := make([]string, 0, len(waiting))
		for _, row := range waiting {
			names = append(names, row.film)
		}
		return "", fmt.Errorf("the movie table still holds films without a date (%s); schedule them with movies_schedule first so nothing lands ahead of them; nothing was proposed", strings.Join(names, ", "))
	}
	for _, film := range films {
		if rows := table.matching(strings.TrimSpace(film)); len(rows) > 0 {
			return "", fmt.Errorf("%q is already in the movie table (%s); a film is never added twice, so nothing was proposed", strings.TrimSpace(film), movieDay(rows[len(rows)-1].date))
		}
	}
	today := s.today()
	turns, err := table.turns("", today)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(pickerName)
	if name == "" {
		name = s.displayNames[principalID]
	}
	picker, err := turns.pickerByInitial(name)
	if err != nil {
		return "", err
	}

	var reserved []movieRow
	for _, row := range table.rows {
		if row.dated && !row.date.Before(today) && row.film == "" && pickerLetter(table.picker(row)) == turns.pickers[picker].letter {
			reserved = append(reserved, row)
		}
	}
	slices.SortStableFunc(reserved, func(a, b movieRow) int { return a.date.Compare(b.date) })

	plan := newMoviePlan(table, turns)
	for index, film := range films {
		film = strings.TrimSpace(film)
		if index >= len(reserved) {
			if err := plan.place(picker, film, nil); err != nil {
				return "", err
			}
			continue
		}
		change, err := movieUpdateChange(reserved[index].page.ID, map[string]any{movieFilmProperty: richTextValue(film)},
			fmt.Sprintf("%s fills the reserved evening of %s", film, movieDay(reserved[index].date)))
		if err != nil {
			return "", err
		}
		plan.changes = append(plan.changes, change)
		plan.filmEnds = append(plan.filmEnds, len(plan.changes))
	}
	if err := plan.checkBundle(); err != nil {
		return "", err
	}
	return s.recordMovieChanges(ctx, principalID, token, plan.changes, "Films: ", "Proposed films: ")
}

func (s *Service) proposeSchedule(ctx context.Context, principalID, token string, table movieTable, film string, date time.Time) (string, error) {
	today := s.today()
	var plan *moviePlan
	if film == "" {
		waiting := table.undated()
		if len(waiting) == 0 {
			return "Every film in the movie table already has a date; nothing was proposed.", nil
		}
		turns, err := table.turns("", today)
		if err != nil {
			return "", err
		}
		plan = newMoviePlan(table, turns)
		for _, row := range waiting {
			if err := plan.placeScheduled(row); err != nil {
				return "", err
			}
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
		if !date.IsZero() {
			change, err := movieDateChange(table, row, date)
			if err != nil {
				return "", err
			}
			return s.recordMovieChanges(ctx, principalID, token, []movieChange{change}, "Dates: ", "Proposed dates: ")
		}
		turns, err := table.turns(row.page.ID, today)
		if err != nil {
			return "", err
		}
		plan = newMoviePlan(table, turns)
		if err := plan.placeScheduled(row); err != nil {
			return "", err
		}
	}
	if err := plan.checkBundle(); err != nil {
		return "", err
	}
	return s.recordMovieChanges(ctx, principalID, token, plan.changes, "Dates: ", "Proposed dates: ")
}

func (p *moviePlan) placeScheduled(row movieRow) error {
	value := p.table.picker(row)
	if pickerLetter(value) == 0 {
		return fmt.Errorf("%q has no picker in column %s, so its turn cannot be told; set it first, nothing was proposed", row.film, moviePickerProperty)
	}
	picker, found := p.turns.pickerOf(value)
	if !found {
		return fmt.Errorf("the picker %q of %q is neither of the current pickers (%s); nothing was proposed", value, row.film, p.turns.names())
	}
	return p.place(picker, row.film, &row)
}

// recordMovieChanges records a call's changes. A call that creates a row proposes every change,
// its updates too, so rejecting the batch leaves the table untouched; a call of updates alone
// records them like any other update.
func (s *Service) recordMovieChanges(ctx context.Context, principalID, token string, changes []movieChange, done, proposed string) (string, error) {
	if s.proposer == nil {
		return "", fmt.Errorf("Notion writes are not available")
	}
	lines := make([]string, 0, len(changes))
	for _, change := range changes {
		lines = append(lines, change.line)
	}
	if slices.ContainsFunc(changes, func(change movieChange) bool { return change.tool == toolCreatePage }) {
		return s.proposeMovieBatch(ctx, principalID, token, changes, lines, proposed)
	}

	results := make([]string, 0, len(changes))
	for index, change := range changes {
		result, err := s.record(ctx, principalID, token, change.tool, change.arguments)
		if err != nil {
			if index > 0 {
				return "", fmt.Errorf("%w; the changes before it were already handled: %s", err, strings.Join(lines[:index], ", "))
			}
			return "", err
		}
		results = append(results, result)
	}
	if !slices.ContainsFunc(results, func(result string) bool { return result != results[0] }) {
		heading := done
		if results[0] == approval.ProposedText {
			heading = proposed
		}
		return heading + strings.Join(lines, ", ") + ". " + results[0], nil
	}
	// Some changes were made and others proposed, so each keeps its own result.
	for index := range lines {
		lines[index] += ": " + results[index]
	}
	return done + "\n" + strings.Join(lines, "\n"), nil
}

// proposeMovieBatch checks every change against its target and then proposes them whole or not at
// all, because an update made at once would survive the user rejecting the creates beside it.
func (s *Service) proposeMovieBatch(ctx context.Context, principalID, token string, changes []movieChange, lines []string, heading string) (string, error) {
	proposals := make([]approval.Proposal, 0, len(changes))
	for _, change := range changes {
		summary, intent, err := s.prepareProposal(ctx, token, change.tool, change.arguments)
		if err != nil {
			return "", err
		}
		proposals = append(proposals, approval.Proposal{Tool: change.tool, Arguments: change.arguments, Summary: summary, Intent: intent})
	}
	if _, err := s.proposer.ProposeAll(ctx, principalID, proposals); err != nil {
		if approval.Refused(err) {
			return "", err
		}
		s.logger.WarnContext(ctx, "notion.proposal.failed", "principal_id", principalID, "tool", toolCreatePage, "error", err)
		return "", fmt.Errorf("the changes could not be recorded; nothing was proposed")
	}
	return heading + strings.Join(lines, ", ") + ". " + approval.ProposedText, nil
}

// proposeRowUpdate records the change as an ordinary notion_update_page call, so it is checked,
// made or proposed, and executed exactly like a property change the model asked for itself.
func (s *Service) proposeRowUpdate(ctx context.Context, principalID, token, pageID string, values map[string]any) (string, error) {
	change, err := movieUpdateChange(pageID, values, "")
	if err != nil {
		return "", err
	}
	return s.record(ctx, principalID, token, change.tool, change.arguments)
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
