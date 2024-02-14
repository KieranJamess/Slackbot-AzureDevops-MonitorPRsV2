// What it can do

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/tkanos/gonfig"
)

var storage PRStorage
var votes = map[int]string{
	10:  "approved",
	5:   "approved (with suggestions)",
	0:   "reset",
	-10: "rejected",
}

type WebhookData struct {
	EventType string `json:"eventType"`
	Resource  struct {
		Repository struct {
			Project struct {
				Name string `json:"name"`
			} `json:"project"`
			Name   string `json:"name"`
			WebURL string `json:"webUrl"`
		} `json:"repository"`
		PullRequestID int    `json:"pullRequestId"`
		Status        string `json:"status"`
		PrTitle       string `json:"title"`
		CreatedBy     struct {
			DisplayName string `json:"displayName"`
			Email       string `json:"uniqueName"`
		} `json:"createdBy"`
		IsDraft   bool        `json:"isDraft"`
		Reviewers []Reviewers `json:reviewers`
		URL       string      `json:url`
	} `json:"resource"`
}

type AutomaticPrMessages struct {
	Projects map[string]ProjectInfo `json:"AutomaticPrMessages"`
}

type ProjectInfo struct {
	ChannelId string `json:"ChannelId"`
}

type PR struct {
	ID               int
	IsDraft          bool
	Status           string
	Reviewers        []Reviewers
	SlackMessageTS   string
	SlackChannel     string
	SentFirstMessage bool
}

type Reviewers struct {
	Vote        int    `json:"vote"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
	IsRequired  bool   `json:"isRequired"`
}

type PRStorage struct {
	sync.Mutex
	prs map[int]PR
}

func (s *PRStorage) Add(pr PR) {
	s.Lock()
	defer s.Unlock()
	s.prs[pr.ID] = pr
}

func (s *PRStorage) Remove(id int) {
	s.Lock()
	defer s.Unlock()
	delete(s.prs, id)
}

func (s *PRStorage) GetByID(id int) (PR, bool) {
	s.Lock()
	defer s.Unlock()
	pr, ok := s.prs[id]
	return pr, ok
}

func handleAzureDevopsWebhookCreated(w http.ResponseWriter, r *http.Request, configuration AutomaticPrMessages, slackClient *slack.Client) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	var data WebhookData
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
		return
	}

	if data.EventType != "git.pullrequest.created" {
		http.Error(w, "EventType is not matching. Event sent to create endpoint", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)

	_, found := storage.GetByID(data.Resource.PullRequestID)

	if !found {
		log.Printf("%d not found in map", data.Resource.PullRequestID)

		projectConfiguration, found := configuration.Projects[data.Resource.Repository.Project.Name]
		if !found {
			log.Printf("No project matching this PR in config.json. Looking for: %s", data.Resource.Repository.Project.Name)
			return
		}

		if data.Resource.IsDraft {
			log.Println("PR is a draft. Will not send slack message at this moment")
			PRState := PR{
				ID:               data.Resource.PullRequestID,
				Status:           data.Resource.Status,
				IsDraft:          data.Resource.IsDraft,
				Reviewers:        data.Resource.Reviewers,
				SlackChannel:     projectConfiguration.ChannelId,
				SentFirstMessage: false,
			}
			storage.Add(PRState)
			return
		}

		found, user := findSlackUser(slackClient, data.Resource.CreatedBy.Email)
		if !found {
			user = data.Resource.CreatedBy.DisplayName
		}

		message := fmt.Sprintf("<@%s> has created a new PR - <%s/pullrequest/%d|*%s*> for *%s*'",
			user,
			data.Resource.Repository.WebURL,
			data.Resource.PullRequestID,
			data.Resource.PrTitle,
			data.Resource.Repository.Name,
		)
		ts := sendSlackMessage(slackClient, projectConfiguration.ChannelId, message, "")

		PRState := PR{
			ID:               data.Resource.PullRequestID,
			Status:           data.Resource.Status,
			IsDraft:          data.Resource.IsDraft,
			Reviewers:        data.Resource.Reviewers,
			SlackMessageTS:   ts,
			SlackChannel:     projectConfiguration.ChannelId,
			SentFirstMessage: true,
		}
		storage.Add(PRState)
	} else {
		log.Println("Webhook received trying to create a PR that has already been tracked")
	}
}

func handleAzureDevopsWebhookUpdates(w http.ResponseWriter, r *http.Request, slackClient *slack.Client) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	var data WebhookData
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
		return
	}

	if data.EventType != "git.pullrequest.updated" {
		http.Error(w, "EventType is not matching. Event sent to update endpoint", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)

	pr, found := storage.GetByID(data.Resource.PullRequestID)
	if !found {
		log.Printf("Can't find PR in storage matching %d", data.Resource.PullRequestID)
		return
	}

	updatedPr := PR{
		Status:    data.Resource.Status,
		IsDraft:   data.Resource.IsDraft,
		Reviewers: data.Resource.Reviewers,
	}

	item, info, changed := comparePRs(pr, updatedPr)

	if changed {
		if item == "draft" && !pr.SentFirstMessage {
			_, id := findSlackUser(slackClient, data.Resource.CreatedBy.Email)
			message := fmt.Sprintf("<@%s> has moved their PR from draft, and is ready to be reviewed - <%s/pullrequest/%d|*%s*> for *%s*'",
				id,
				data.Resource.Repository.WebURL,
				data.Resource.PullRequestID,
				data.Resource.PrTitle,
				data.Resource.Repository.Name,
			)
			ts := sendSlackMessage(slackClient, pr.SlackChannel, message, "")
			pr.SlackMessageTS = ts
			pr.SentFirstMessage = true
			storage.Add(pr)
		} else {
			sendSlackMessage(slackClient, pr.SlackChannel, info, pr.SlackMessageTS)

			if updatedPr.Status == "completed" {
				// delete message
				threadMessages, err := getThreadMessages(slackClient, pr.SlackChannel, pr.SlackMessageTS)
				if err != nil {
					log.Println("Error retrieving thread messages:", err)
					return
				}
				if err := deleteThreadMessages(slackClient, pr.SlackChannel, threadMessages); err != nil {
					log.Println("Error deleting thread messages:", err)
					return
				}
				log.Printf("Removing %d from being tracked", pr.ID)
				storage.Remove(pr.ID)
			} else {
				pr.Status = updatedPr.Status
				pr.IsDraft = updatedPr.IsDraft
				pr.Reviewers = updatedPr.Reviewers
				storage.Add(pr)
			}
		}
	}
}

func comparePRs(prInState, updatedPr PR) (change string, message string, changed bool) {
	if prInState.Status != updatedPr.Status {
		message = fmt.Sprintf("Status has changed to: *%s*", updatedPr.Status)
		return "status", message, true
	}

	if prInState.IsDraft != updatedPr.IsDraft {
		if updatedPr.IsDraft {
			message = "PR has now been marked as a draft"
		} else {
			message = "PR has been unmarked as a draft"
		}
		return "draft", message, true
	}

	// Check if the number of reviewers has changed
	if len(prInState.Reviewers) != len(updatedPr.Reviewers) {
		// If it has, find the new reviewer and return their details
		for _, updatedReviewer := range updatedPr.Reviewers {
			found := false
			for _, prInStateReviewer := range prInState.Reviewers {
				if prInStateReviewer.UniqueName == updatedReviewer.UniqueName {
					found = true
					break
				}
			}
			if !found {
				message = fmt.Sprintf("*%s* has *%s* this PR", updatedReviewer.DisplayName, votes[updatedReviewer.Vote])
				return "reviewer", message, true
			}
		}
	}

	// If the number of reviewers hasn't changed, compare each reviewer's vote
	for i, prInStateReviewer := range prInState.Reviewers {
		updatedReviewer := updatedPr.Reviewers[i]

		// If they're a required reviewer, we dont want to trigger a changed vote message
		if prInStateReviewer.Vote != updatedReviewer.Vote && !prInStateReviewer.IsRequired {
			message = fmt.Sprintf("*%s* has changed their review from *%s* to *%s*", updatedReviewer.DisplayName, votes[prInStateReviewer.Vote], votes[updatedReviewer.Vote])
			return "reviewer", message, true
		} else if prInStateReviewer.Vote != updatedReviewer.Vote {
			message = fmt.Sprintf("A required reviewer - *%s* has *%s* your PR ", updatedReviewer.DisplayName, votes[updatedReviewer.Vote])
			return "reviewer", message, true
		}
	}

	return "none", "", false
}

func sendSlackMessage(slackClient *slack.Client, channelID string, message string, messageTs string) (message_ts string) {
	_, message_ts, err := slackClient.PostMessage(channelID, slack.MsgOptionText(message, false), slack.MsgOptionTS(messageTs))
	if err != nil {
		log.Fatalf("Error sending message: %v", err)
	}
	return message_ts
}

func findSlackUser(slackClient *slack.Client, email string) (foundUser bool, userid string) {
	user, err := slackClient.GetUserByEmail(email)
	if err != nil {
		log.Println("Error retrieving slack user", err)
	}

	if user == nil {
		return false, ""
	} else {
		return true, user.ID
	}
}

func getThreadMessages(slackClient *slack.Client, channelID string, parentTimestamp string) ([]slack.Message, error) {
	params := slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: parentTimestamp,
	}
	messages, _, _, err := slackClient.GetConversationReplies(&params)

	if err != nil {
		return nil, err
	}
	return messages, nil
}

func deleteThreadMessages(slackClient *slack.Client, channelID string, messages []slack.Message) error {
	for _, msg := range messages {
		_, _, err := slackClient.DeleteMessage(channelID, msg.Timestamp)
		if err != nil {
			return err
		}
	}
	return nil
}

func main() {
	godotenv.Load(".env")

	var slack_token string
	slack_token = os.Getenv("SLACK_ACCESS_TOKEN")
	if slack_token == "" {
		log.Fatal("Missing critical environment variable")
	}

	configuration := AutomaticPrMessages{}
	err := gonfig.GetConf("config.json", &configuration)
	if err != nil {
		log.Fatal("Error loading configuration:", err)
	}

	if len(configuration.Projects) > 0 {
		fmt.Println("-------------------\n[GLOBAL] Automatic PR configuration below")
		for key, value := range configuration.Projects {
			fmt.Println("-------------------\nProject:", key)
			fmt.Println("ChannelId:", value.ChannelId)

		}
		fmt.Println("-------------------")
	} else {
		fmt.Println("[GLOBAL] No automatic PR configuration")
	}

	slackClient := slack.New(slack_token, slack.OptionDebug(true))

	storage = PRStorage{
		prs: make(map[int]PR),
	}

	http.HandleFunc("/azuredevops/create", func(w http.ResponseWriter, r *http.Request) {
		handleAzureDevopsWebhookCreated(w, r, configuration, slackClient)
	})

	http.HandleFunc("/azuredevops/updates", func(w http.ResponseWriter, r *http.Request) {
		handleAzureDevopsWebhookUpdates(w, r, slackClient)
	})

	// http.HandleFunc("/azuredevops/comments", func(w http.ResponseWriter, r *http.Request) {
	// 	handleAzureDevopsWebhookComments(w, r, slackClient)
	// })

	// can we comment on merged or closed PRs? Do we need to check for that

	fmt.Println("[GLOBAL] Server listening on port 80...")
	http.ListenAndServe(":80", nil)
}
