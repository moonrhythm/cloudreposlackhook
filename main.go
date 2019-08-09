package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/acoshift/configfile"
	"google.golang.org/api/option"
)

var (
	config   = configfile.NewReader("config")
	mode     = config.String("mode") // push, pull
	slackURL = config.String("slack_url")
)

func main() {
	if mode == "push" {
		port := config.StringDefault("port", "8080")
		startPush(port)
		return
	}

	startPull()
}

func startPull() {
	projectID := config.String("project_id")
	subscription := config.String("subscription")

	ctx := context.Background()

	client, err := pubsub.NewClient(ctx, "", option.WithScopes(pubsub.ScopePubSub))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	fmt.Printf("subscribe to %s/%s\n", projectID, subscription)
	err = client.SubscriptionInProject(subscription, projectID).
		Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
			defer msg.Ack()

			fmt.Println("received message")

			var d data
			err := json.Unmarshal(msg.Data, &d)
			if err != nil {
				return
			}

			err = processData(&d)
			if err != nil {
				msg.Nack()
				return
			}
		})
	if err != nil {
		log.Fatal(err)
		return
	}
}

func startPush(port string) {
	fmt.Println("Listening on", port)
	http.ListenAndServe(":"+port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)

		if r.Method != http.MethodPost {
			return
		}
		mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "application/json" {
			return
		}

		var msg struct {
			Message struct {
				Data []byte `json:"data,omitempty"`
				ID   string `json:"id"`
			} `json:"message"`
			Subscription string `json:"subscription"`
		}
		err := json.NewDecoder(r.Body).Decode(&msg)
		if err != nil {
			log.Println(err)
			return
		}

		if msg.Subscription == "" {
			log.Println("invalid message")
			return
		}

		fmt.Printf("received push message from %s\n", msg.Subscription)

		var d data
		err = json.Unmarshal(msg.Message.Data, &d)
		if err != nil {
			log.Println("invalid message body")
			return
		}

		processData(&d)
	}))
}

func processData(d *data) error {
	for _, ev := range d.RefUpdateEvent.RefUpdates {
		err := processUpdateEvent(d, &ev)
		if err != nil {
			return err
		}
	}
	return nil
}

func processUpdateEvent(d *data, ev *updateEvent) error {
	color := updateTypeColor[ev.UpdateType]
	if color == "" {
		return nil
	}

	var (
		projectID string
		repoName  string
	)
	{
		xs := strings.SplitN(d.Name, "/", 4)
		if len(xs) != 4 {
			return nil
		}
		projectID = xs[1]
		repoName = xs[3]
	}

	commitURL := fmt.Sprintf("https://source.cloud.google.com/%s/%s/+/%s", projectID, repoName, ev.NewID)

	// https://source.developers.google.com/p/moonrhythm-core/r/makro-accountconnect/8de10e0d546f9d86799597770a373de0a1c2ec8d

	return sendSlackMessage(&slackMsg{
		Attachments: []slackAttachment{
			{
				Fallback: fmt.Sprintf("%s:%s",
					d.Name,
					ev.RefName,
				),
				Color:      color,
				Title:      "Cloud Repo",
				TitleLink:  commitURL,
				AuthorName: d.RefUpdateEvent.Email,
				AuthorIcon: gravatarURL(d.RefUpdateEvent.Email),
				Fields: []slackField{
					{
						Title: "Repository",
						Value: d.Name,
					},
					{
						Title: "Branch",
						Value: ev.RefName,
					},
					{
						Title: "Email",
						Value: d.RefUpdateEvent.Email,
					},
					{
						Title: "Update Type",
						Value: ev.UpdateType,
					},
					{
						Title: "Commit SHA",
						Value: ev.NewID,
					},
				},
			},
		},
	})
}

var updateTypeColor = map[string]string{
	"CREATE":                  "#2e77ff",
	"UPDATE_FAST_FORWARD":     "#60ff55",
	"UPDATE_NON_FAST_FORWARD": "#ff2e2e",
	"DELETE":                  "#ff6d2e",
}

type data struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	EventTime      time.Time
	RefUpdateEvent struct {
		Email      string                 `json:"email"`
		RefUpdates map[string]updateEvent `json:"refUpdates"`
	} `json:"refUpdateEvent"`
}

type updateEvent struct {
	RefName    string `json:"refName"`
	UpdateType string `json:"updateType"`
	OldID      string `json:"oldId"`
	NewID      string `json:"newId"`
}

type slackMsg struct {
	Text        string            `json:"text,omitempty"`
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

type slackAttachment struct {
	Fallback   string       `json:"fallback"`
	Color      string       `json:"color"`
	Pretext    string       `json:"pretext"`
	AuthorName string       `json:"author_name,omitempty"`
	AuthorLink string       `json:"author_link,omitempty"`
	AuthorIcon string       `json:"author_icon,omitempty"`
	Title      string       `json:"title"`
	TitleLink  string       `json:"title_link"`
	Text       string       `json:"text"`
	Fields     []slackField `json:"fields"`
	ImageURL   string       `json:"image_url,omitempty"`
	ThumbURL   string       `json:"thumb_url,omitempty"`
	Footer     string       `json:"footer,omitempty"`
	FooterIcon string       `json:"footer_icon,omitempty"`
	Timestamp  int64        `json:"ts"`
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

var client = http.Client{
	Timeout: 5 * time.Second,
}

func gravatarURL(email string) string {
	if email == "" {
		return ""
	}
	s := md5.Sum([]byte(email))
	return "http://gravatar.com/avatar/" + hex.EncodeToString(s[:])
}

func sendSlackMessage(message *slackMsg) error {
	if slackURL == "" {
		return nil
	}

	buf := bytes.Buffer{}
	err := json.NewEncoder(&buf).Encode(message)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, slackURL, &buf)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("response not ok")
	}
	return nil
}
