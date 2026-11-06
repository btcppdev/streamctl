package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"streamctl/internal/db"
)

const defaultProductionTemplateJSON = `{"version":1,"settings":{},"segments":[]}`

type productionTemplateListItem struct {
	db.ProductionTemplate
	SegmentCount int
	UpdatedLabel string
	URL          string
}

type productionTemplatesPage struct {
	Nav       productionNavView
	Templates []productionTemplateListItem
	Deleted   int
	Error     string
}

type productionTemplateEditPage struct {
	RecordingNotices     []productionRecordingNotice
	RecordingTalkNotices map[string]productionRecordingNotice
	Nav                  productionNavView
	ItemID               int64
	Conference           string
	Name                 string
	DefinitionJSON       template.JS
	Kind                 string
	BackURL              string
	SaveAction           string
	DuplicateAction      string
	DeleteAction         string
	AllowDynamic         bool
	CanDelete            bool
	Saved                bool
	SelectName           bool
	Error                string
}

type productionTemplateDefinition struct {
	Version  int                `json:"version"`
	Settings json.RawMessage    `json:"settings"`
	Segments *[]json.RawMessage `json:"segments"`
}

type productionTemplateSettings struct {
	Width            *int    `json:"width"`
	Height           *int    `json:"height"`
	FPS              *int    `json:"fps"`
	TransitionMS     *int    `json:"transitionMs"`
	ImageMS          *int    `json:"imageMs"`
	AudioSampleRate  *int    `json:"audioSampleRate"`
	VideoEncoder     *string `json:"videoEncoder"`
	VideoBitrate     *string `json:"videoBitrate"`
	AudioBitrate     *string `json:"audioBitrate"`
	KeyframeInterval *int    `json:"keyframeInterval"`
	NVENCCQ          *int    `json:"nvencCq"`
	SoftwarePreset   *string `json:"softwarePreset"`
	SoftwareCRF      *int    `json:"softwareCrf"`
}

type productionTemplateAudio struct {
	Src          string   `json:"src"`
	Mode         string   `json:"mode"`
	In           *string  `json:"in"`
	GainDB       *float64 `json:"gainDb"`
	SourceGainDB *float64 `json:"sourceGainDb"`
}

type productionSegmentMetadata struct {
	TalkTitle    string `json:"talkTitle"`
	TalkSchedule string `json:"talkSchedule,omitempty"`
}

type productionTemplateImageSegment struct {
	Streamctl  *productionSegmentMetadata `json:"streamctl,omitempty"`
	Type       string                     `json:"type"`
	Src        string                     `json:"src"`
	DurationMS *int                       `json:"durationMs"`
	Overlay    *string                    `json:"overlay"`
	Audio      *productionTemplateAudio   `json:"audio"`
}

type productionTemplateVideoSegment struct {
	Streamctl  *productionSegmentMetadata `json:"streamctl,omitempty"`
	Type       string                     `json:"type"`
	Src        string                     `json:"src"`
	In         *string                    `json:"in"`
	Out        *string                    `json:"out"`
	Transcribe *bool                      `json:"transcribe"`
	Overlay    *string                    `json:"overlay"`
	Audio      *productionTemplateAudio   `json:"audio"`
}

type productionTemplateTalkCutsSegment struct {
	Type    string  `json:"type"`
	Overlay *string `json:"overlay"`
}

type productionTemplateTalkCardSegment struct {
	Type       string `json:"type"`
	DurationMS *int   `json:"durationMs"`
}

var productionTemplateBitrate = regexp.MustCompile(`^[1-9][0-9]*(?:[kKmMgG])?$`)
var productionTemplateTimecode = regexp.MustCompile(`^(\d{2,}):(\d{2}):(\d{2})(?:\.(\d{3}))?$`)

func (h *Handler) productionTemplates(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	page := productionTemplatesPage{Deleted: queryInt(r, "deleted")}
	page.Nav.Active = "templates"
	page.Nav.Action = "/production/templates"
	page.Nav.Conference = conference
	page.Nav.Conferences, page.Error = h.productionConferences(r.Context())
	if conference == "" {
		h.render(w, r, "production_templates.html", page)
		return
	}
	if !validProductionConference(conference) {
		h.renderStatus(w, r, http.StatusBadRequest, "production_templates.html", page)
		return
	}
	items, err := h.DB.ListProductionTemplates(conference)
	if err != nil {
		page.Error = err.Error()
	} else {
		for _, item := range items {
			var definition productionTemplateDefinition
			_ = json.Unmarshal([]byte(item.JSON), &definition)
			page.Templates = append(page.Templates, productionTemplateListItem{
				ProductionTemplate: item,
				SegmentCount:       productionTemplateSegmentCount(definition),
				UpdatedLabel:       item.UpdatedAt.Format("2006-01-02 15:04"),
				URL:                productionTemplateURL(conference, item.ID),
			})
		}
	}
	h.render(w, r, "production_templates.html", page)
}

func (h *Handler) productionTemplateEdit(w http.ResponseWriter, r *http.Request) {
	conference := selectedProductionConference(r)
	rememberProductionConference(w, r, conference)
	id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("id")), 10, 64)
	if !validProductionConference(conference) || err != nil || id <= 0 {
		http.Error(w, "conference and template ID are required", http.StatusBadRequest)
		return
	}
	item, err := h.DB.ProductionTemplate(id, conference)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conferences, conferenceError := h.productionConferences(r.Context())
	page := productionTemplateEditPage{
		ItemID: item.ID, Conference: item.Conference, Name: item.Name, Kind: "template",
		BackURL:    "/production/templates?conference=" + url.QueryEscape(conference),
		SaveAction: "/production/templates/save", DuplicateAction: "/production/templates/duplicate",
		DeleteAction: "/production/templates/delete", AllowDynamic: true,
		CanDelete: true,
		Saved:     r.URL.Query().Get("saved") == "1", SelectName: r.URL.Query().Get("created") == "1", Error: conferenceError,
	}
	page.Nav = productionNavView{Active: "templates", Action: "/production/templates", Conference: conference, Conferences: conferences}
	encoded, err := json.Marshal(json.RawMessage(item.JSON))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.DefinitionJSON = template.JS(encoded)
	h.render(w, r, "production_template_edit.html", page)
}

func (h *Handler) productionTemplateCreate(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	name := strings.TrimSpace(r.FormValue("name"))
	if !validProductionConference(conference) {
		http.Error(w, "conference is required", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = "Untitled template"
	}
	id, err := h.DB.CreateProductionTemplate(conference, name, defaultProductionTemplateJSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, productionTemplateURL(conference, id)+"&created=1", http.StatusSeeOther)
}

func (h *Handler) productionTemplateSave(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	name := strings.TrimSpace(r.FormValue("name"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if !validProductionConference(conference) || name == "" || err != nil || id <= 0 {
		http.Error(w, "template ID, conference, and name are required", http.StatusBadRequest)
		return
	}
	rawDefinition := r.FormValue("definition")
	if rawDefinition == "" {
		rawDefinition = r.FormValue("template")
	}
	definition, err := validateProductionTemplate([]byte(rawDefinition))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.DB.UpdateProductionTemplate(id, conference, name, definition); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, productionTemplateURL(conference, id)+"&saved=1", http.StatusSeeOther)
}

func (h *Handler) productionTemplateDuplicate(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if !validProductionConference(conference) || err != nil || id <= 0 {
		http.Error(w, "template ID and conference are required", http.StatusBadRequest)
		return
	}
	item, err := h.DB.ProductionTemplate(id, conference)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyID, err := h.DB.CreateProductionTemplate(conference, item.Name+" copy", item.JSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, productionTemplateURL(conference, copyID), http.StatusSeeOther)
}

func (h *Handler) productionTemplateDelete(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if !validProductionConference(conference) || err != nil || id <= 0 {
		http.Error(w, "template ID and conference are required", http.StatusBadRequest)
		return
	}
	if err := h.DB.DeleteProductionTemplate(id, conference); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/production/templates?conference="+url.QueryEscape(conference), http.StatusSeeOther)
}

func (h *Handler) productionTemplatesDelete(w http.ResponseWriter, r *http.Request) {
	conference := strings.TrimSpace(r.FormValue("conference"))
	ids, err := selectedProductionIDs(r)
	if !validProductionConference(conference) || err != nil {
		http.Error(w, "conference and selected templates are required", http.StatusBadRequest)
		return
	}
	deleted, err := h.DB.DeleteProductionTemplates(conference, ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/production/templates?conference=%s&deleted=%d", url.QueryEscape(conference), deleted), http.StatusSeeOther)
}

func productionTemplateURL(conference string, id int64) string {
	return "/production/templates/edit?conference=" + url.QueryEscape(conference) + "&id=" + strconv.FormatInt(id, 10)
}

func validateProductionTemplate(raw []byte) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", errors.New("template definition is required")
	}
	if len(raw) > maxRenderManifestBytes {
		return "", fmt.Errorf("template exceeds %d bytes", maxRenderManifestBytes)
	}
	var definition productionTemplateDefinition
	if err := decodeStrictJSON(raw, &definition); err != nil {
		return "", fmt.Errorf("invalid template JSON: %w", err)
	}
	if definition.Version != 1 {
		return "", fmt.Errorf("unsupported template version %d", definition.Version)
	}
	if definition.Settings == nil || bytes.Equal(bytes.TrimSpace(definition.Settings), []byte("null")) {
		return "", errors.New("template settings must be an object")
	}
	if err := validateProductionTemplateSettings(definition.Settings); err != nil {
		return "", err
	}
	if definition.Segments == nil {
		return "", errors.New("template segments must be an array")
	}
	for i, rawSegment := range *definition.Segments {
		if err := validateProductionTemplateSegment(rawSegment); err != nil {
			return "", fmt.Errorf("segment %d: %w", i+1, err)
		}
	}
	canonical, err := json.MarshalIndent(definition, "", "  ")
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func productionTemplateSegmentCount(definition productionTemplateDefinition) int {
	if definition.Segments == nil {
		return 0
	}
	return len(*definition.Segments)
}

func validateProductionTemplateSettings(raw json.RawMessage) error {
	var settings productionTemplateSettings
	if err := decodeStrictJSON(raw, &settings); err != nil {
		return fmt.Errorf("invalid settings: %w", err)
	}
	positive := map[string]*int{
		"width": settings.Width, "height": settings.Height, "fps": settings.FPS,
		"transitionMs": settings.TransitionMS, "imageMs": settings.ImageMS,
		"audioSampleRate": settings.AudioSampleRate, "keyframeInterval": settings.KeyframeInterval,
	}
	for name, value := range positive {
		if value != nil && *value <= 0 {
			return fmt.Errorf("setting %s must be greater than zero", name)
		}
	}
	if settings.VideoEncoder != nil && !containsString([]string{"auto", "software", "videotoolbox", "nvenc"}, *settings.VideoEncoder) {
		return errors.New("setting videoEncoder is invalid")
	}
	if settings.VideoBitrate != nil && *settings.VideoBitrate != "" && !productionTemplateBitrate.MatchString(*settings.VideoBitrate) {
		return errors.New("setting videoBitrate is invalid")
	}
	if settings.AudioBitrate != nil && !productionTemplateBitrate.MatchString(*settings.AudioBitrate) {
		return errors.New("setting audioBitrate is invalid")
	}
	if settings.NVENCCQ != nil && (*settings.NVENCCQ < 0 || *settings.NVENCCQ > 51) {
		return errors.New("setting nvencCq must be between 0 and 51")
	}
	if settings.SoftwareCRF != nil && (*settings.SoftwareCRF < 0 || *settings.SoftwareCRF > 51) {
		return errors.New("setting softwareCrf must be between 0 and 51")
	}
	if settings.SoftwarePreset != nil && strings.TrimSpace(*settings.SoftwarePreset) == "" {
		return errors.New("setting softwarePreset may not be empty")
	}
	return nil
}

func validateProductionTemplateSegment(raw json.RawMessage) error {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.Type == "" {
		return errors.New("type is required")
	}
	switch header.Type {
	case "image":
		var segment productionTemplateImageSegment
		if err := decodeStrictJSON(raw, &segment); err != nil {
			return err
		}
		if strings.TrimSpace(segment.Src) == "" {
			return errors.New("src is required")
		}
		if segment.DurationMS != nil && *segment.DurationMS <= 0 {
			return errors.New("durationMs must be greater than zero")
		}
		if err := validateProductionTemplateMediaSource(segment.Src, "image"); err != nil {
			return err
		}
		return validateProductionTemplateExtras(segment.Overlay, segment.Audio)
	case "video", "chunkedVideo":
		var segment productionTemplateVideoSegment
		if err := decodeStrictJSON(raw, &segment); err != nil {
			return err
		}
		if strings.TrimSpace(segment.Src) == "" {
			return errors.New("src is required")
		}
		if err := validateProductionTemplateMediaSource(segment.Src, "video"); err != nil {
			return err
		}
		if err := validateProductionTemplateExtras(segment.Overlay, segment.Audio); err != nil {
			return err
		}
		return validateProductionTemplateWindow(segment.In, segment.Out)
	case "streamctl.talkCuts":
		var segment productionTemplateTalkCutsSegment
		if err := decodeStrictJSON(raw, &segment); err != nil {
			return err
		}
		return validateProductionTemplateExtras(segment.Overlay, nil)
	case "streamctl.talkCard":
		var segment productionTemplateTalkCardSegment
		if err := decodeStrictJSON(raw, &segment); err != nil {
			return err
		}
		if segment.DurationMS != nil && *segment.DurationMS <= 0 {
			return errors.New("durationMs must be greater than zero")
		}
		return nil
	default:
		return fmt.Errorf("unsupported type %q", header.Type)
	}
}

func validateProductionTemplateExtras(overlay *string, audio *productionTemplateAudio) error {
	if overlay != nil {
		if err := validateProductionTemplateMediaSource(*overlay, "image"); err != nil {
			return fmt.Errorf("overlay: %w", err)
		}
	}
	if audio == nil {
		return nil
	}
	if err := validateProductionTemplateMediaSource(audio.Src, "audio"); err != nil {
		return fmt.Errorf("audio: %w", err)
	}
	if audio.Mode != "" && audio.Mode != "replace" && audio.Mode != "mix" {
		return errors.New("audio mode must be replace or mix")
	}
	if audio.In != nil {
		if _, err := parseProductionTemplateTimecode(*audio.In); err != nil {
			return fmt.Errorf("audio in: %w", err)
		}
	}
	if audio.Mode != "mix" && audio.SourceGainDB != nil && *audio.SourceGainDB != 0 {
		return errors.New("audio sourceGainDb is only valid in mix mode")
	}
	return nil
}

func validateProductionTemplateMediaSource(source, kind string) error {
	if err := validateProductionTemplateSource(source); err != nil {
		return err
	}
	if actual := mediaFileKind(source); actual != kind {
		return fmt.Errorf("source must be a supported %s file", kind)
	}
	return nil
}

func validateProductionTemplateSource(source string) error {
	if source != strings.TrimSpace(source) || strings.ContainsAny(source, "\r\n\x00") {
		return fmt.Errorf("source %q is not a valid object key", source)
	}
	clean := path.Clean(source)
	if source == "" || strings.HasPrefix(source, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != source {
		return fmt.Errorf("source %q must be a relative bucket object key", source)
	}
	return nil
}

func validateProductionTemplateWindow(in, out *string) error {
	var inMS, outMS int64
	var err error
	if in != nil {
		inMS, err = parseProductionTemplateTimecode(*in)
		if err != nil {
			return fmt.Errorf("in: %w", err)
		}
	}
	if out != nil {
		outMS, err = parseProductionTemplateTimecode(*out)
		if err != nil {
			return fmt.Errorf("out: %w", err)
		}
	}
	if in != nil && out != nil && outMS <= inMS {
		return errors.New("out must be later than in")
	}
	return nil
}

func parseProductionTemplateTimecode(value string) (int64, error) {
	match := productionTemplateTimecode.FindStringSubmatch(value)
	if match == nil {
		return 0, errors.New("must use HH:MM:SS or HH:MM:SS.mmm")
	}
	hours, _ := strconv.ParseInt(match[1], 10, 64)
	minutes, _ := strconv.ParseInt(match[2], 10, 64)
	seconds, _ := strconv.ParseInt(match[3], 10, 64)
	if minutes >= 60 || seconds >= 60 {
		return 0, errors.New("minutes and seconds must be between 00 and 59")
	}
	millis := int64(0)
	if match[4] != "" {
		millis, _ = strconv.ParseInt(match[4], 10, 64)
	}
	return ((hours*60+minutes)*60+seconds)*1000 + millis, nil
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("must contain one JSON value")
	}
	return nil
}
