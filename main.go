package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/phin1x/go-ipp"
	"github.com/slack-go/slack/socketmode"

	"github.com/charmbracelet/log"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

type PendingPrint struct {
	File             string
	PrinterMessageTs string
}

var PendingPrints map[string]PendingPrint
var MyID string

type Printer struct {
	Queue       string
	DisplayName string
	Note        string
	Reaction    string
}

var Printers = []Printer{
	{Queue: "KONICA-MINOLTA-magicolor-1690MF", DisplayName: "Farebne", Note: "KONICA MINOLTA MAGICOLOR!", Reaction: "rainbow"},
	{Queue: "Hermiona", DisplayName: "Čiernobielo", Note: "Hermiona", Reaction: "black_circle"},
}

func main() {
	appToken := os.Getenv("PRINTBOT_APP_TOKEN")
	botToken := os.Getenv("PRINTBOT_BOT_TOKEN")

	PendingPrints = make(map[string]PendingPrint)

	api := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
		slack.OptionLog(log.WithPrefix("slack api").StandardLog()),
	)

	resp, err := api.AuthTest()
	if err != nil {
		log.Error("Could not authenticate against Slack API.", "err", err)
		os.Exit(1)
	}

	MyID = resp.UserID

	client := socketmode.New(api, socketmode.OptionLog(log.WithPrefix("slack socket").StandardLog()))
	handler := socketmode.NewSocketmodeHandler(client)

	handler.HandleEvents(slackevents.Message, handleMessage)
	handler.HandleEvents(slackevents.ReactionAdded, handleReaction)

	err = handler.RunEventLoop()
	if err != nil {
		log.Error("Error while running event loop.", "err", err)
		os.Exit(1)
	}
}

func sendMessage(client *socketmode.Client, ch string, msg string) {
	_, _, err := client.Client.PostMessage(ch, slack.MsgOptionText(msg, false))
	if err != nil {
		log.Error("Failed posting message", "err", err)
	}
}

func addPendingPrint(channel string, file string, client *socketmode.Client) {
	PendingPrints[channel] = PendingPrint{
		File:             file,
		PrinterMessageTs: askPrinter(channel, client),
	}
}

func askPrinter(channel string, client *socketmode.Client) string {
	printers := []string{}
	for _, printer := range Printers {
		printers = append(printers, fmt.Sprintf("- :%s: *%s* _(%s)_", printer.Reaction, printer.DisplayName, printer.Note))
	}

	_, timestamp, err := client.PostMessage(channel, slack.MsgOptionText("Ako to chceš vytlačiť?\n\n"+strings.Join(printers, "\n"), false))
	if err != nil {
		log.Error("Failed posting message", "err", err)
		return ""
	}

	for _, r := range []string{"rainbow", "black_circle"} {
		err = client.AddReaction(r, slack.NewRefToMessage(channel, timestamp))
		if err != nil {
			log.Error("Could not add reaction.", "err", err)
		}
	}

	return timestamp
}

func handleMessage(evt *socketmode.Event, client *socketmode.Client) {
	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		log.Warn("Ignoring malformed event.")
		return
	}

	client.Ack(*evt.Request)

	ev, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		log.Warn("Ignoring malformed event.")
		return
	}

	if ev.User == MyID {
		return
	}

	if ev.SubType != "" && ev.SubType != "file_share" {
		return
	}

	if len(ev.Files) != 1 {
		sendMessage(client, ev.Channel, "Pošli mi prosím ťa jedno PDF, ktoré chceš vytlačiť.")
		return
	}

	addPendingPrint(ev.Channel, ev.Files[0].URLPrivateDownload, client)
}

func handleReaction(evt *socketmode.Event, client *socketmode.Client) {
	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		log.Warn("Ignoring malformed event.")
		return
	}

	client.Ack(*evt.Request)

	ev, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.ReactionAddedEvent)
	if !ok {
		log.Warn("Ignoring malformed event.")
		return
	}

	if ev.User == MyID {
		return
	}

	channel := ev.Item.Channel
	print, exists := PendingPrints[channel]
	if !exists {
		return
	}

	if ev.Item.Timestamp != print.PrinterMessageTs {
		return
	}

	var printer Printer
	for _, p := range Printers {
		if p.Reaction == ev.Reaction {
			printer = p
		}
	}

	if printer.Reaction == "" {
		return
	}

	_, _, err := client.DeleteMessage(channel, ev.Item.Timestamp)
	if err != nil {
		log.Error("Could not delete message.", "err", err)
	}

	var buf bytes.Buffer
	err = client.GetFile(print.File, &buf)
	if err != nil {
		log.Error("Could not download file.", "err", err)
		sendMessage(client, channel, "Niečo sa pokazilo. :(")
		return
	}

	doc := ipp.Document{
		Document: &buf,
		Size:     buf.Len(),
		Name:     "printbot.pdf",
		MimeType: "application/pdf",
	}
	ippClient := ipp.NewIPPClient(os.Getenv("PRINTBOT_CUPS_IP"), 631, "", "", false)
	job, err := ippClient.PrintJob(doc, printer.Queue, map[string]interface{}{})
	if err != nil {
		log.Error("Could not send print job.", "printer", printer.Queue, "err", err)
		sendMessage(client, channel, "Niečo sa pokazilo. :(")
		return
	}

	log.Info("Sending print job.", "user", ev.User, "file", print.File, "printer", printer.Queue, "job", job)
	sendMessage(client, channel, "Súbor odoslaný na tlač.")
}
