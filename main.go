package main

import (
	"bytes"
	"encoding/json"
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

var (
	PendingPrints map[string]PendingPrint
	MyID          string
	IPPClient     *ipp.CUPSClient
)

type Printer struct {
	Queue       string `json:"queue"`
	DisplayName string `json:"display_name"`
	Note        string `json:"note"`
	Reaction    string `json:"reaction"`
}

type SlackConfig struct {
	AppToken string `json:"app_token"`
	BotToken string `json:"bot_token"`
}

type CUPSConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	TLS      bool   `json:"tls"`
}

type Configuration struct {
	Printers []Printer   `json:"printers"`
	Slack    SlackConfig `json:"slack"`
	CUPS     CUPSConfig  `json:"cups"`
}

var Config Configuration

func main() {
	var err error
	configPath := os.Getenv("PRINTBOT_CONFIG_FILE")
	if configPath == "" {
		configPath = "config.json"
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		log.Error("Could not open config file.", "err", err, "file", configPath)
		os.Exit(1)
	}

	err = json.Unmarshal(configData, &Config)
	if err != nil {
		log.Error("Could not read config file.", "err", err)
		os.Exit(1)
	}

	IPPClient = ipp.NewCUPSClient(Config.CUPS.Host, Config.CUPS.Port, Config.CUPS.Username, Config.CUPS.Password, Config.CUPS.TLS)

	PendingPrints = make(map[string]PendingPrint)

	api := slack.New(
		Config.Slack.BotToken,
		slack.OptionAppLevelToken(Config.Slack.AppToken),
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
	_, _, err := client.PostMessage(ch, slack.MsgOptionText(msg, false))
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
	for _, printer := range Config.Printers {
		printers = append(printers, fmt.Sprintf("- :%s: *%s* _(%s)_", printer.Reaction, printer.DisplayName, printer.Note))
	}

	_, timestamp, err := client.PostMessage(channel, slack.MsgOptionText("Ako to chceš vytlačiť?\n\n"+strings.Join(printers, "\n"), false))
	if err != nil {
		log.Error("Failed posting message", "err", err)
		return ""
	}

	for _, p := range Config.Printers {
		err = client.AddReaction(p.Reaction, slack.NewRefToMessage(channel, timestamp))
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

	if len(ev.Message.Files) != 1 {
		sendMessage(client, ev.Channel, "Pošli mi prosím ťa jedno PDF, ktoré chceš vytlačiť.")
		return
	}

	addPendingPrint(ev.Channel, ev.Message.Files[0].URLPrivateDownload, client)
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
	for _, p := range Config.Printers {
		if p.Reaction == ev.Reaction {
			printer = p
		}
	}

	if printer.Reaction == "" {
		log.Warn("Unknown reaction", "reaction", ev.Reaction)
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
	job, err := IPPClient.PrintJob(doc, printer.Queue, map[string]any{})
	if err != nil {
		log.Error("Could not send print job.", "printer", printer.Queue, "err", err)
		sendMessage(client, channel, "Niečo sa pokazilo. :(")
		return
	}

	log.Info("Sending print job.", "user", ev.User, "file", print.File, "printer", printer.Queue, "job", job)
	sendMessage(client, channel, "Súbor odoslaný na tlač.")
}
