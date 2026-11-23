package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const articlesRoute = "/api/articles"
const adminArticlesRoute = "/api/admin/articles"
const articleSummaryColumns = "id, title, summary, published_at, cover_image, CASE WHEN published_at IS NOT NULL THEN featured_position END"
const articleColumns = articleSummaryColumns + ", body, body_images"

var errInvalidFeaturedArticles = errors.New("featured articles must be published")

type articleSummary struct {
	ID               pgtype.UUID   `json:"id"`
	Title            string        `json:"title"`
	Summary          string        `json:"summary"`
	PublishedAt      apiTime       `json:"publishedAt"`
	CoverImage       *articleImage `json:"coverImage"`
	FeaturedPosition *int16        `json:"featuredPosition"`
}

type article struct {
	articleSummary
	Body       string             `json:"body"`
	BodyImages []articleBodyImage `json:"bodyImages"`
}

type articleImage struct {
	Image string `json:"image"`
	Alt   string `json:"alt"`
}

type articleBodyImage struct {
	articleImage
	Caption        string `json:"caption,omitempty"`
	AfterParagraph *int   `json:"afterParagraph"`
}

type articleMedia struct {
	CoverImage jsontext.Value     `json:"coverImage"`
	BodyImages []articleBodyImage `json:"bodyImages"`
}

type featuredArticles struct {
	IDs []pgtype.UUID `json:"ids"`
}

type articleListResponse struct {
	Status string           `json:"status"`
	Length int              `json:"length"`
	Data   []articleSummary `json:"data"`
}

type articleInput struct {
	Title       *string        `json:"title"`
	Summary     *string        `json:"summary"`
	Body        *string        `json:"body"`
	IsPublished *bool          `json:"isPublished"`
	Media       jsontext.Value `json:"media"`
}

func registerPublicArticles(mux *http.ServeMux, db database) {
	mux.HandleFunc("GET "+articlesRoute, listArticles(db, true))
	mux.HandleFunc("GET "+articlesRoute+"/{id}", getArticle(db, true))
}

func (auth *accountAuth) registerAdminArticles(mux *http.ServeMux) {
	detail := adminArticlesRoute + "/{id}"
	mux.HandleFunc("GET "+adminArticlesRoute, auth.requireModerator(listArticles(auth.db, false)))
	mux.HandleFunc("GET "+detail, auth.requireModerator(getArticle(auth.db, false)))
	mux.HandleFunc("POST "+adminArticlesRoute, auth.requireModerator(auth.saveArticle))
	mux.HandleFunc("PUT "+detail, auth.requireModerator(auth.saveArticle))
	mux.HandleFunc("PUT "+adminArticlesRoute+"/featured", auth.requireModerator(auth.saveFeaturedArticles))
}

func listArticles(db database, publishedOnly bool) http.HandlerFunc {
	query := "SELECT " + articleSummaryColumns + " FROM article"
	if publishedOnly {
		query += " WHERE published_at IS NOT NULL"
	}
	query += " ORDER BY published_at DESC NULLS LAST, id"

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		entries, err := queryStructs[articleSummary](r.Context(), db, query)
		if err != nil {
			writeArticleError(w, err)
			return
		}
		writeJSON(w, articleListResponse{Status: "success", Length: len(entries), Data: entries})
	}
}

func getArticle(db database, publishedOnly bool) http.HandlerFunc {
	query := "SELECT " + articleColumns + " FROM article WHERE id = $1"
	if publishedOnly {
		query += " AND published_at IS NOT NULL"
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		entry, err := scanArticle(db.QueryRow(r.Context(), query, id))
		writeArticleResult(w, entry, err)
	}
}

func readArticleInput(w http.ResponseWriter, r *http.Request) (articleInput, bool) {
	var input articleInput
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return input, false
	}
	err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &input)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeText(w, http.StatusRequestEntityTooLarge, "Слишком большой запрос")
	case err != nil || input.Title == nil || input.Summary == nil || input.Body == nil || input.IsPublished == nil:
		writeText(w, http.StatusBadRequest, "Некорректные данные статьи")
	case !validArticleText(*input.Title, 160) || strings.TrimSpace(*input.Title) == "":
		writeText(w, http.StatusBadRequest, "Введите заголовок до 160 символов")
	case !validArticleText(*input.Summary, 350):
		writeText(w, http.StatusBadRequest, "Краткое описание должно содержать не более 350 символов")
	case !validArticleText(*input.Body, 30000):
		writeText(w, http.StatusBadRequest, "Текст статьи должен содержать не более 30 000 символов")
	case *input.IsPublished && strings.TrimSpace(*input.Body) == "":
		writeText(w, http.StatusBadRequest, "Введите текст статьи для публикации")
	default:
		return input, true
	}
	return input, false
}

func validArticleText(value string, limit int) bool {
	return !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= limit
}

func (auth *accountAuth) saveArticle(w http.ResponseWriter, r *http.Request) {
	var id pgtype.UUID
	if r.Method == http.MethodPut {
		var ok bool
		id, ok = requestID(w, r)
		if !ok {
			return
		}
	}
	input, ok := readArticleInput(w, r)
	if !ok {
		return
	}
	var cover *articleImage
	var bodyImages any
	if input.Media != nil {
		var media articleMedia
		if err := json.Unmarshal(input.Media, &media); err != nil ||
			json.Unmarshal(media.CoverImage, &cover) != nil ||
			(cover != nil && !validArticleImage(*cover)) ||
			!validArticleBodyImages(*input.Body, media.BodyImages) {
			writeText(w, http.StatusBadRequest, "Некорректные данные изображений статьи")
			return
		}
		bodyImages = media.BodyImages
	}
	query := `INSERT INTO article (title, summary, body, published_at, cover_image, body_images)
		VALUES ($1, $2, $3, CASE WHEN $4 THEN clock_timestamp() END, $5, COALESCE($6::jsonb, '[]'::jsonb))
		RETURNING ` + articleColumns
	args := []any{*input.Title, *input.Summary, *input.Body, *input.IsPublished, cover, bodyImages}
	if id.Valid {
		query = `UPDATE article SET title = $1, summary = $2, body = $3,
			published_at = CASE WHEN $4 THEN COALESCE(published_at, clock_timestamp()) END,
			featured_position = CASE WHEN $4 AND published_at IS NOT NULL THEN featured_position END,
			cover_image = CASE WHEN $6::jsonb IS NULL THEN cover_image ELSE $5::jsonb END,
			body_images = COALESCE($6::jsonb, body_images)
			WHERE id = $7 RETURNING ` + articleColumns
		args = append(args, id)
	}

	var entry article
	ctx := r.Context()
	err := pgx.BeginTxFunc(ctx, auth.fullDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		var err error
		entry, err = scanArticle(tx.QueryRow(ctx, query, args...))
		return err
	})
	writeArticleResult(w, entry, err)
}

func validArticleImage(image articleImage) bool {
	if strings.TrimSpace(image.Alt) == "" || !validArticleText(image.Alt, 200) {
		return false
	}
	file, ok := strings.CutPrefix(image.Image, "/images/")
	if !ok {
		return false
	}
	key, extension, _ := strings.Cut(file, ".")
	switch extension {
	case "jpg", "png", "gif", "webp":
		id, err := parseID(key)
		return err == nil && id.String() == key
	default:
		return false
	}
}

func validArticleBodyImages(body string, images []articleBodyImage) bool {
	if images == nil || len(images) > 10 {
		return false
	}
	paragraphs := 0
	for _, line := range strings.FieldsFunc(body, isArticleLineBreak) {
		if strings.TrimSpace(line) != "" {
			paragraphs++
		}
	}
	previous := 0
	for _, image := range images {
		position := image.AfterParagraph
		if position == nil || *position < previous || *position > paragraphs ||
			!validArticleImage(image.articleImage) || !validArticleText(image.Caption, 240) {
			return false
		}
		previous = *position
	}
	return true
}

func isArticleLineBreak(r rune) bool {
	return r == '\r' || r == '\n'
}

func (auth *accountAuth) saveFeaturedArticles(w http.ResponseWriter, r *http.Request) {
	const invalidSelection = "Выберите до трёх разных опубликованных статей"
	if !isJSONRequest(r) {
		writeText(w, http.StatusBadRequest, "Content type error")
		return
	}
	var input struct {
		IDs []string `json:"ids"`
	}
	err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBodySize), &input)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeText(w, http.StatusRequestEntityTooLarge, "Слишком большой запрос")
		return
	case err != nil || input.IDs == nil || len(input.IDs) > 3:
		writeText(w, http.StatusBadRequest, invalidSelection)
		return
	}
	ids := make([]pgtype.UUID, 0, len(input.IDs))
	for _, value := range input.IDs {
		id, err := parseID(value)
		if err != nil || slices.Contains(ids, id) {
			writeText(w, http.StatusBadRequest, invalidSelection)
			return
		}
		ids = append(ids, id)
	}

	ctx := r.Context()
	err = pgx.BeginTxFunc(ctx, auth.fullDB, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		// Serialize replacement with other replacements and publication changes.
		if _, err := tx.Exec(ctx, "LOCK TABLE article IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return err
		}
		var published int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM article WHERE id = ANY($1) AND published_at IS NOT NULL", ids).Scan(&published)
		if err != nil {
			return err
		}
		if published != len(ids) {
			return errInvalidFeaturedArticles
		}
		if _, err := tx.Exec(ctx, "UPDATE article SET featured_position = NULL WHERE featured_position IS NOT NULL"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE article SET featured_position = picks.position
			FROM unnest($1::uuid[]) WITH ORDINALITY AS picks(id, position)
			WHERE article.id = picks.id`, ids)
		return err
	})
	if errors.Is(err, errInvalidFeaturedArticles) {
		writeText(w, http.StatusBadRequest, invalidSelection)
		return
	}
	if err != nil {
		writeArticleError(w, err)
		return
	}
	writeJSON(w, singleResponse{Status: "success", Data: featuredArticles{IDs: ids}})
}

func scanArticle(row pgx.Row) (article, error) {
	var entry article
	err := row.Scan(&entry.ID, &entry.Title, &entry.Summary, &entry.PublishedAt, &entry.CoverImage,
		&entry.FeaturedPosition, &entry.Body, &entry.BodyImages)
	return entry, err
}

func writeArticleResult(w http.ResponseWriter, entry article, err error) {
	if err != nil {
		writeArticleError(w, err)
		return
	}
	writeJSON(w, singleResponse{Status: "success", Data: entry})
}

func writeArticleError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	slog.Error("article query failed")
	writeText(w, http.StatusInternalServerError, "Не удалось сохранить или загрузить статью")
}
