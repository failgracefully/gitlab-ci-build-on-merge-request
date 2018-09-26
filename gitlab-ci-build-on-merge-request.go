package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type requestBody struct {
	ObjectKind string `json:"object_kind"` // merge_request
	Project    struct {
		Name string `json:"name"`
	} `json:"project"`
	ObjectAttributes struct {
		SourceBranch    string `json:"source_branch"`
		SourceProjectId int    `json:"source_project_id"`
		Id              int    `json:"id"`
		Action          string `json:"action"`
		State           string `json:"state"` // merged, opened or closed
		LastCommit      struct {
			Id string `json:"id"`
		} `json:"last_commit"`
		WorkInProgress bool `json:"work_in_progress"`
	} `json:"object_attributes"`
	Labels  []label `json:"labels"`
	Changes struct {
		Labels struct {
			Previous []label `json:"previous"`
			Current  []label `json:"current"`
		} `json:"labels"`
	} `json:"changes"`
}

type trigger struct {
	Id    int    `json:"id"`
	Token string `json:"token"`
	Owner struct {
		Id       int    `json:"id"`
		Username string `json:"username"`
	} `json:"owner"`
}

type build struct {
	Id     int    `json:"id"`
	Status string `json:"status"`
}

type label struct {
	Id    int    `json:"id"`
	Title string `json:"title"`
}

func printUsageAndExit(msg string) {
	if msg != "" {
		fmt.Fprintf(os.Stderr, msg+"\n\n")
	}
	flag.Usage()
	os.Exit(1)
}

func printWarning(msg string) {
	if msg != "" {
		fmt.Fprintf(os.Stderr, msg+"\n\n")
	}
}

// todo: make sure private token does not leak through response/log
func main() {
	var baseURL = flag.String("url", "", "URL (e.g. http://gitlab.com)")
	var privateTokenGlobal = flag.String("private_token", "", "Authorization Token (e.g. XXxXXx0xxxXXXxXxXxxX)")
	var port = flag.Int("port", 8080, "Port")
	flag.Parse()
	if *baseURL == "" {
		printUsageAndExit("Error: --url is required")
	}
	if *privateTokenGlobal == "" {
		printWarning("Warning: --private_token is not set")
	}
	http.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		queryPrivateToken := r.URL.Query().Get("private_token")
		var privateToken *string
		if queryPrivateToken != "" {
			privateToken = &queryPrivateToken
		} else {
			privateToken = privateTokenGlobal
		}
		if *privateToken == "" {
			fmt.Fprintf(os.Stderr, "Error: private_token is required\n")
		}
		var requestBody = &requestBody{}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			log.Printf("[BOMR] WARN: Failed to deserialize request body (%s)", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		fmt.Println(requestBody)

		requestBodyAsByteArray, _ := json.Marshal(requestBody)
		fmt.Printf("%s\n", requestBodyAsByteArray)
		log.Printf("[BOMR] INFO: Received %s", string(requestBodyAsByteArray))

		// do not trigger build if merge request is WIP or merged/closed
		if requestBody.ObjectKind != "merge_request" || requestBody.ObjectAttributes.State != "opened" ||
			requestBody.ObjectAttributes.WorkInProgress {
			return
		}

		if requestBody.Changes.Labels.Current == nil {

			// do not trigger if build for commit was already triggered
			buildsUrl := fmt.Sprintf(
				"%s/api/v4/projects/%d/repository/commits/%s/statuses?private_token=%s",
				*baseURL,
				requestBody.ObjectAttributes.SourceProjectId,
				requestBody.ObjectAttributes.LastCommit.Id,
				*privateToken)
			buildsRes, err := http.Get(buildsUrl)
			if err != nil {
				log.Printf("[BOMR] WARN: %s", err.Error())
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer buildsRes.Body.Close()
			if buildsRes.StatusCode >= 400 {
				log.Printf("[BOMR] WARN: GET %s resulted in %d", buildsUrl, buildsRes.StatusCode)
				http.Error(w, fmt.Sprintf("[BOMR] GET %s resulted in %d", buildsUrl, buildsRes.StatusCode),
					http.StatusInternalServerError)
				return
			}
			var builds []build
			if err := json.NewDecoder(buildsRes.Body).Decode(&builds); err != nil {
				log.Printf("[BOMR] WARN: Failed to deserialize response of GET %s (%s)", buildsUrl, err.Error())
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(builds) > 0 {
				for _, build := range builds {
					if build.Status != "skipped" {
						log.Printf("[BOMR] INFO: %s build skipped (reason: build %d is in \"%s\" status)",
							requestBody.ObjectAttributes.LastCommit.Id, build.Id, build.Status)
						return
					}
				}
			}
		} else {
			log.Print("[BOMR] Labels changed, skip commit specific check")
		}
		trigger, err := resolveTrigger(*baseURL, *privateToken, requestBody.ObjectAttributes.SourceProjectId)
		if err != nil {
			log.Printf("[BOMR] WARN: %s", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		triggerUrl := fmt.Sprintf(
			"%s/api/v4/projects/%d/trigger/pipeline?ref=%s&token=%s",
			*baseURL,
			requestBody.ObjectAttributes.SourceProjectId,
			requestBody.ObjectAttributes.SourceBranch,
			trigger.Token)

		var labels string
		for _, label := range requestBody.Labels {
			if IsValidUUID(label.Title) {
				labels += label.Title + ","
			}
		}
		labels = strings.Trim(labels, ",")

		triggerRes, err := http.PostForm(triggerUrl, url.Values{"variables[MERGEREQUEST_ID]": {fmt.Sprintf("%d", requestBody.ObjectAttributes.Id)},
			"variables[MERGEREQUEST_LABELS]": {labels}})
		if err != nil {
			log.Printf("[BOMR] WARN: %s", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer triggerRes.Body.Close()
		// todo: follow redirects
		if triggerRes.StatusCode != 201 {
			log.Printf("[BOMR] WARN: POST %s resulted in %d", triggerUrl, triggerRes.StatusCode)
			http.Error(w, fmt.Sprintf("[BOMR] POST %s resulted in %d", triggerUrl, triggerRes.StatusCode),
				http.StatusInternalServerError)
			return
		}
		log.Printf("[BOMR] INFO: Triggered build of %s#%s", requestBody.Project.Name,
			requestBody.ObjectAttributes.SourceBranch)
	})
	log.Printf(fmt.Sprintf("[BOMR] INFO: Listening on port %d", *port))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

func IsValidUUID(uuid string) bool {
	r := regexp.MustCompile("[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-4[a-fA-F0-9]{3}-[8|9|aA|bB][a-fA-F0-9]{3}-[a-fA-F0-9]{12}$")
	return r.MatchString(uuid)
}

func resolveTrigger(baseURL string, privateToken string, projectId int) (*trigger, error) {
	fullURL := fmt.Sprintf("%s/api/v4/projects/%d/triggers?private_token=%s", baseURL, projectId, privateToken)
	res, err := http.Get(fullURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s resulted in %d", fullURL, res.StatusCode)
	}
	var triggers []trigger
	if err := json.NewDecoder(res.Body).Decode(&triggers); err != nil {
		return nil, fmt.Errorf("Failed to deserialize response of GET %s (%s)", fullURL, err.Error())
	}
	if len(triggers) == 0 {
		res, err := http.PostForm(fullURL, url.Values{
			"description": {"gitlab-ci-build-on-merge-request"},
		})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != 201 {
			return nil, fmt.Errorf("POST %s resulted in %d", fullURL, res.StatusCode)
		}
		var t trigger
		if err := json.NewDecoder(res.Body).Decode(&t); err != nil {
			return nil, fmt.Errorf("Failed to deserialize response of POST %s (%s)", fullURL, err.Error())
		}
		triggers = []trigger{t}
	}
	trigger := triggers[0]
	if trigger.Owner.Id == 0 { // legacy trigger (without owner)
		takeOwnershipURL := fmt.Sprintf("%s/api/v4/projects/%d/triggers/%d/take_ownership?private_token=%s",
			baseURL, projectId, trigger.Id, privateToken)
		res, err := http.PostForm(takeOwnershipURL, url.Values{})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("POST %s resulted in %d", takeOwnershipURL, res.StatusCode)
		}
		if err := json.NewDecoder(res.Body).Decode(&trigger); err != nil {
			return nil, fmt.Errorf("Failed to deserialize response of POST %s (%s)", takeOwnershipURL, err.Error())
		}
	}
	return &trigger, nil
}
