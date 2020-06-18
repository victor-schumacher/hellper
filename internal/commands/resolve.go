package commands

import (
	"context"
	"hellper/internal/concurrence"
	"strconv"
	"strings"
	"sync"
	"time"

	"hellper/internal/bot"
	calendar "hellper/internal/calendar"
	"hellper/internal/config"
	"hellper/internal/log"
	"hellper/internal/model"

	"github.com/slack-go/slack"
)

var patternStringDate = "2013-04-01 22:43"

// ResolveIncidentDialog opens a dialog on Slack, so the user can resolve an incident
func ResolveIncidentDialog(client bot.Client, triggerID string) error {
	description := &slack.TextInputElement{
		DialogInput: slack.DialogInput{
			Label:       "Description",
			Name:        "incident_description",
			Type:        "textarea",
			Placeholder: "Description eg. The incident was resolved after #PR fix",
			Optional:    false,
		},
		MaxLength: 500,
	}
	statusIO := &slack.TextInputElement{
		DialogInput: slack.DialogInput{
			Label:       "Status.io link",
			Name:        "status_io",
			Type:        "text",
			Placeholder: "status.io/xxxx",
			Optional:    false,
		},
		Subtype: slack.InputSubtypeURL,
	}

	postMortemMeeting := &slack.DialogInputSelect{
		DialogInput: slack.DialogInput{
			Label:       "Can I schedule a Post Mortem meeting?",
			Name:        "post_mortem_meeting",
			Type:        "select",
			Placeholder: "Post Mortem Meeting",
			Optional:    false,
		},
		Value: "false",
		Options: []slack.DialogSelectOption{
			{
				Label: "Yes",
				Value: "true",
			},
			{
				Label: "No",
				Value: "false",
			},
		},
		OptionGroups: []slack.DialogOptionGroup{},
	}

	dialog := slack.Dialog{
		CallbackID:     "inc-resolve",
		Title:          "Resolve an Incident",
		SubmitLabel:    "Resolve",
		NotifyOnCancel: false,
		Elements: []slack.DialogElement{
			statusIO,
			description,
			postMortemMeeting,
		},
	}

	return client.OpenDialog(triggerID, dialog)
}

// ResolveIncidentByDialog resolves an incident after receiving data from a Slack dialog
func ResolveIncidentByDialog(ctx context.Context, client bot.Client, logger log.Logger, repository model.Repository, incidentDetails bot.DialogSubmission, calendar calendar.Calendar) error {
	logger.Info(
		ctx,
		"command/resolve.ResolveIncidentByDialog",
		log.NewValue("incident_resolve_details", incidentDetails),
	)

	var (
		now               = time.Now().UTC()
		channelID         = incidentDetails.Channel.ID
		channelName       = incidentDetails.Channel.Name
		userID            = incidentDetails.User.ID
		userName          = incidentDetails.User.Name
		submissions       = incidentDetails.Submission
		description       = submissions.IncidentDescription
		statusPageURL     = submissions.StatusIO
		postMortemMeeting = submissions.PostMortemMeeting
		notifyOnResolve   = config.Env.NotifyOnResolve
		productChannelID  = config.Env.ProductChannelID

		calendarEvent *model.Event
	)

	incident := model.Incident{
		ChannelId:           channelID,
		EndTimestamp:        &now,
		DescriptionResolved: description,
		StatusPageUrl:       statusPageURL,
	}

	err := repository.ResolveIncident(ctx, &incident)
	if err != nil {
		logger.Error(
			ctx,
			"command/resolve.ResolveIncidentByDialog repository.resolve_incident error",
			log.NewValue("error", err),
		)
		return err
	}

	hasPostMortemMeeting, err := strconv.ParseBool(postMortemMeeting)
	if err != nil {
		logger.Error(
			ctx,
			"command/resolve.ResolveIncidentByDialog strconv.parse_bool error",
			log.NewValue("error", err),
		)
		return err
	}

	if hasPostMortemMeeting {

		calendarEvent, err = getCalendarEvent(ctx, logger, repository, calendar, incident.EndTimestamp, channelName, channelID)
		if err != nil {
			logger.Error(
				ctx,
				"command/resolve.ResolveIncidentByDialog get_calendar_event error",
				log.NewValue("error", err),
			)
			return err
		}
	}

	channelAttachment := createResolveChannelAttachment(incident, userName, calendarEvent.EventURL)
	privateAttachment := createResolvePrivateAttachment(incident, calendarEvent)

	var waitgroup sync.WaitGroup
	defer waitgroup.Wait()

	concurrence.WithWaitGroup(&waitgroup, func() {
		postAndPinMessage(client, channelID, "", channelAttachment)
	})
	if notifyOnResolve {
		concurrence.WithWaitGroup(&waitgroup, func() {
			postAndPinMessage(client, productChannelID, "", channelAttachment)
		})
	}
	postMessage(client, userID, "", privateAttachment)

	return nil
}

func setMeetingDate(ctx context.Context, logger log.Logger, d *time.Time, postMortemGapDays int, timezone string) (string, string, error) {
	previewPostMortemDate := d.AddDate(0, 0, postMortemGapDays)

	switch previewPostMortemDate.Weekday() {
	case time.Saturday:
		postMortemGapDays += 2
	case time.Sunday:
		postMortemGapDays++
	default:
		break
	}

	utc, err := time.LoadLocation(timezone)
	if err != nil {
		logger.Error(
			ctx,
			"command/resolve.setMeetingDate time.load_location error",
			log.NewValue("error", err),
		)
		return "", "", err
	}

	setMeetingHour := time.Date(d.Year(), d.Month(), d.Day(), 15, 0, 0, 0, utc)

	startMeeting := setMeetingHour.AddDate(0, 0, postMortemGapDays)
	endMeeting := startMeeting.Add(time.Hour).Format(time.RFC3339)

	return startMeeting.Format(time.RFC3339), endMeeting, err
}

func getCalendarEvent(
	ctx context.Context,
	logger log.Logger,
	repository model.Repository,
	calendar calendar.Calendar,
	t *time.Time,
	channelName string,
	channelID string,
) (*model.Event, error) {
	startMeeting, endMeeting, err := setMeetingDate(ctx, logger, t, config.Env.PostmortemGapDays, config.Env.Timezone)
	if err != nil {
		logger.Error(
			ctx,
			"command/resolve.getCalendarEvent set_meeting_date error",
			log.NewValue("error", err),
		)
		return nil, err
	}

	summary := "[Post Mortem] " + channelName
	emails := []string{} //This will be filled in V2

	inc, err := repository.GetIncident(ctx, channelID)
	if err != nil {
		logger.Error(
			ctx,
			"command/resolve.getCalendarEvent repository.get_incident error",
			log.NewValue("error", err),
		)
		return nil, err
	}

	calendarEvent, err := calendar.CreateCalendarEvent(ctx, startMeeting, endMeeting, summary, inc.CommanderEmail, emails)
	if err != nil {
		logger.Error(
			ctx,
			"command/resolve.getCalendarEvent calendar.create_calendar_event error",
			log.NewValue("error", err),
		)
		return nil, err
	}
	return calendarEvent, err
}

func createResolveChannelAttachment(inc model.Incident, userName string, postMortemMeetingURL string) slack.Attachment {
	endDateText := inc.EndTimestamp.Format(time.RFC3339)

	var messageText strings.Builder
	messageText.WriteString("The Incident <#" + inc.ChannelId + "> has been resolved by <@" + userName + ">\n\n")
	messageText.WriteString("*End date:* <#" + endDateText + ">\n")
	messageText.WriteString("*Status.io link:* `" + inc.StatusPageUrl + "`\n")
	messageText.WriteString("*Description:* `" + inc.DescriptionResolved + "`\n")
	if postMortemMeetingURL != "" {
		messageText.WriteString("*Post Mortem Meeting Link:* `" + postMortemMeetingURL + "`\n")
	} else {
		messageText.WriteString("\n\n")
		postMortemMeetingURL = "The Meeting is not scheduled."
	}

	return slack.Attachment{
		Pretext:  "The Incident <#" + inc.ChannelId + "> has been resolved by <@" + userName + ">",
		Fallback: messageText.String(),
		Text:     "",
		Color:    "#1164A3",
		Fields: []slack.AttachmentField{
			{
				Title: "Incident",
				Value: "<#" + inc.ChannelId + ">",
			},
			{
				Title: "End date",
				Value: endDateText,
			},
			{
				Title: "Status.io link",
				Value: inc.StatusPageUrl,
			},
			{
				Title: "Description",
				Value: inc.DescriptionResolved,
			},
			{
				Title: "Post Mortem meeting link",
				Value: postMortemMeetingURL,
			},
		},
	}
}

func createResolvePrivateAttachment(inc model.Incident, event *model.Event) slack.Attachment {
	var (
		privateText       strings.Builder
		postMortemMessage string
	)
	privateText.WriteString("The Incident <#" + inc.ChannelId + "> has been resolved by you\n\n")
	privateText.WriteString("*Status.io:* Be sure to update the incident status on" + inc.StatusPageUrl + "\n")
	if (&model.Event{}) == event {
		privateText.WriteString("\n\n")
		privateText.WriteString("*Post Mortem:* Don't forget to bookmark Post Mortem for the incident <#" + inc.ChannelId + ">\n")
		postMortemMessage = "A Post Mortem Meeting was not schedule, be sure to fill up the Post Mortem document in the next 7 days."
	} else {
		privateText.WriteString("*Post Mortem Meeting Link:* `" + event.EventURL + "`\n\n")
		postMortemMessage = "I have scheduled a Post Mortem Meeting for you!\nIt will be on `" + event.Start.Format("02/01/2006 15:04:05 MST") + "`.\nHere is the link: `" + event.EventURL + "`\n"
	}

	return slack.Attachment{
		Pretext:  "The Incident <#" + inc.ChannelId + "> has been resolved by you",
		Fallback: privateText.String(),
		Text:     "",
		Color:    "#1164A3",
		Fields: []slack.AttachmentField{
			{
				Title: "Status.io",
				Value: "Be sure to update the incident status on " + inc.StatusPageUrl,
			},
			{
				Title: "Post Mortem",
				Value: postMortemMessage,
			},
		},
	}
}
