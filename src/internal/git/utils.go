package git

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	netHttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/defenseunicorns/zarf/src/config"
	"github.com/defenseunicorns/zarf/src/internal/k8s"
	"github.com/defenseunicorns/zarf/src/internal/message"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

type Credential struct {
	Path string
	Auth http.BasicAuth
}

var (
	// For further explanation: https://regex101.com/r/zq64q4/1
	gitURLRegex = regexp.MustCompile(`^(?P<proto>[a-z]+:\/\/)(?P<hostPath>.+?)\/(?P<repo>[\w\-\.]+?)(?P<git>\.git)?(?P<atRef>@(?P<ref>[\w\-\.]+))?$`)
)

// MutateGitURlsInText Changes the giturl hostname to use the repository Zarf is configured to use
func MutateGitUrlsInText(host string, text string, gitUser string) string {
	extractPathRegex := regexp.MustCompilePOSIX(`https?://[^/]+/(.*\.git)`)
	output := extractPathRegex.ReplaceAllStringFunc(text, func(match string) string {
		output, err := transformURL(host, match, gitUser)
		if err != nil {
			message.Warnf("Unable to transform the git url, using the original url we have: %s", match)
			output = match
		}
		return output
	})
	return output
}

func transformURLtoRepoName(url string) (string, error) {
	matches := gitURLRegex.FindStringSubmatch(url)
	idx := gitURLRegex.SubexpIndex

	if len(matches) == 0 {
		// Unable to find a substring match for the regex
		return "", fmt.Errorf("unable to get extract the repoName from the url %s", url)
	}

	repoName := matches[idx("repo")]
	// NOTE: We remove the .git and protocol so that https://zarf.dev/repo.git and http://zarf.dev/repo
	// resolve to the same repp (as they would in real life)
	sanitizedURL := fmt.Sprintf("%s/%s%s", matches[idx("hostPath")], repoName, matches[idx("atRef")])

	// Add crc32 hash of the repoName to the end of the repo
	table := crc32.MakeTable(crc32.IEEE)
	checksum := crc32.Checksum([]byte(sanitizedURL), table)
	newRepoName := fmt.Sprintf("%s-%d", repoName, checksum)

	return newRepoName, nil
}

func transformURL(baseURL string, url string, username string) (string, error) {
	repoName, err := transformURLtoRepoName(url)
	if err != nil {
		return "", err
	}
	output := fmt.Sprintf("%s/%s/%s", baseURL, username, repoName)
	message.Debugf("Rewrite git URL: %s -> %s", url, output)
	return output, nil
}

func credentialFilePath() string {
	homePath, _ := os.UserHomeDir()
	return filepath.Join(homePath, ".git-credentials")
}

func credentialParser() []Credential {
	credentialsPath := credentialFilePath()
	var credentials []Credential

	credentialsFile, _ := os.Open(credentialsPath)
	defer func(credentialsFile *os.File) {
		err := credentialsFile.Close()
		if err != nil {
			message.Debugf("Unable to load an existing git credentials file: %#v", err)
		}
	}(credentialsFile)

	scanner := bufio.NewScanner(credentialsFile)
	for scanner.Scan() {
		gitUrl, err := url.Parse(scanner.Text())
		if err != nil {
			continue
		}
		password, _ := gitUrl.User.Password()
		credential := Credential{
			Path: gitUrl.Host,
			Auth: http.BasicAuth{
				Username: gitUrl.User.Username(),
				Password: password,
			},
		}
		credentials = append(credentials, credential)
	}

	return credentials
}

func FindAuthForHost(baseUrl string) Credential {
	// Read the ~/.git-credentials file
	gitCreds := credentialParser()

	// Will be nil unless a match is found
	var matchedCred Credential

	// Look for a match for the given host path in the creds file
	for _, gitCred := range gitCreds {
		hasPath := strings.Contains(baseUrl, gitCred.Path)
		if hasPath {
			matchedCred = gitCred
			break
		}
	}

	return matchedCred
}

// removeLocalBranchRefs removes all refs that are local branches
// It returns a slice of references deleted
func removeLocalBranchRefs(gitDirectory string) ([]*plumbing.Reference, error) {
	return removeReferences(
		gitDirectory,
		func(ref *plumbing.Reference) bool {
			return ref.Name().IsBranch()
		},
	)
}

// removeOnlineRemoteRefs removes all refs pointing to the online-upstream
// It returns a slice of references deleted
func removeOnlineRemoteRefs(gitDirectory string) ([]*plumbing.Reference, error) {
	return removeReferences(
		gitDirectory,
		func(ref *plumbing.Reference) bool {
			return strings.HasPrefix(ref.Name().String(), onlineRemoteRefPrefix)
		},
	)
}

// removeHeadCopies removes any refs that aren't HEAD but have the same hash
// It returns a slice of references deleted
func removeHeadCopies(gitDirectory string) ([]*plumbing.Reference, error) {
	message.Debugf("Remove head copies for %s", gitDirectory)
	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return nil, fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to identify references when getting the repo's head: %w", err)
	}

	headHash := head.Hash().String()
	return removeReferences(
		gitDirectory,
		func(ref *plumbing.Reference) bool {
			// Don't ever remove tags
			return !ref.Name().IsTag() && ref.Hash().String() == headHash
		},
	)
}

// removeReferences removes references based on a provided callback
// removeReferences does not allow you to delete HEAD
// It returns a slice of references deleted
func removeReferences(
	gitDirectory string,
	shouldRemove func(*plumbing.Reference) bool,
) ([]*plumbing.Reference, error) {
	message.Debugf("Remove git references %s", gitDirectory)
	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return nil, fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	references, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("failed to identify references when getting the repo's references: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to identify head: %w", err)
	}

	var removedRefs []*plumbing.Reference
	err = references.ForEach(func(ref *plumbing.Reference) error {
		refIsNotHeadOrHeadTarget := ref.Name() != plumbing.HEAD && ref.Name() != head.Name()
		// Run shouldRemove inline here to take advantage of short circuit
		// evaluation as to not waste a cycle on HEAD
		if refIsNotHeadOrHeadTarget && shouldRemove(ref) {
			err = repo.Storer.RemoveReference(ref.Name())
			if err != nil {
				return err
			}
			removedRefs = append(removedRefs, ref)
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to remove references: %w", err)
	}

	return removedRefs, nil
}

// addRefs adds a provided arbitrary list of references to a repo
// It is intended to be used with references returned by a Remove function
func addRefs(gitDirectory string, refs []*plumbing.Reference) error {
	message.Debugf("Add git refs %s", gitDirectory)
	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	for _, ref := range refs {
		err = repo.Storer.SetReference(ref)
		if err != nil {
			return fmt.Errorf("failed to add references: %w", err)
		}
	}

	return nil
}

// deleteBranchIfExists ensures the provided branch name does not exist
func deleteBranchIfExists(gitDirectory string, branchName plumbing.ReferenceName) error {
	message.Debugf("Delete branch %s for %s if it exists", branchName.String(), gitDirectory)

	repo, err := git.PlainOpen(gitDirectory)
	if err != nil {
		return fmt.Errorf("not a valid git repo or unable to open: %w", err)
	}

	// Deletes the branch by name
	err = repo.DeleteBranch(branchName.Short())
	if err != nil && err != git.ErrBranchNotFound {
		return fmt.Errorf("failed to delete branch: %w", err)
	}

	// Delete reference too
	err = repo.Storer.RemoveReference(branchName)
	if err != nil && err != git.ErrInvalidReference {
		return fmt.Errorf("failed to delete branch reference: %w", err)
	}

	return nil
}

// CreateReadOnlyUser uses the Gitea API to create a non-admin zarf user
func CreateReadOnlyUser() error {
	// Establish a git tunnel to send the repo
	tunnel := k8s.NewZarfTunnel()
	tunnel.Connect(k8s.ZarfGit, false)
	defer tunnel.Close()

	tunnelUrl := tunnel.Endpoint()
	zarfState := config.GetState()

	// Create json representation of the create-user request body
	createUserBody := map[string]interface{}{
		"username":             zarfState.GitServer.PullUsername,
		"password":             zarfState.GitServer.PullPassword,
		"email":                "zarf-reader@localhost.local",
		"must_change_password": false,
	}
	createUserData, err := json.Marshal(createUserBody)
	if err != nil {
		return err
	}

	// Send API request to create the user
	createUserEndpoint := fmt.Sprintf("http://%s/api/v1/admin/users", tunnelUrl)
	createUserRequest, _ := netHttp.NewRequest("POST", createUserEndpoint, bytes.NewBuffer(createUserData))
	out, err := DoHttpThings(createUserRequest, zarfState.GitServer.PushUsername, zarfState.GitServer.PushPassword)
	message.Debugf("POST %s:\n%s", createUserEndpoint, string(out))
	if err != nil {
		return err
	}

	// Make sure the user can't create their own repos or orgs
	updateUserBody := map[string]interface{}{
		"login_name":                zarfState.GitServer.PushUsername,
		"max_repo_creation":         0,
		"allow_create_organization": false,
	}
	updateUserData, _ := json.Marshal(updateUserBody)
	updateUserEndpoint := fmt.Sprintf("http://%s/api/v1/admin/users/%s", tunnelUrl, zarfState.GitServer.PullUsername)
	updateUserRequest, _ := netHttp.NewRequest("PATCH", updateUserEndpoint, bytes.NewBuffer(updateUserData))
	out, err = DoHttpThings(updateUserRequest, zarfState.GitServer.PushUsername, zarfState.GitServer.PushPassword)
	message.Debugf("PATCH %s:\n%s", updateUserEndpoint, string(out))
	return err
}

func addReadOnlyUserToRepo(tunnelUrl, repo string) error {
	// Add the readonly user to the repo
	addColabBody := map[string]string{
		"permission": "read",
	}
	addColabData, err := json.Marshal(addColabBody)
	if err != nil {
		return err
	}

	// Send API request to add a user as a read-only collaborator to a repo
	addColabEndpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/collaborators/%s", tunnelUrl, config.GetState().GitServer.PushUsername, repo, config.GetState().GitServer.PullUsername)
	addColabRequest, _ := netHttp.NewRequest("PUT", addColabEndpoint, bytes.NewBuffer(addColabData))
	out, err := DoHttpThings(addColabRequest, config.GetState().GitServer.PushUsername, config.GetState().GitServer.PushPassword)
	message.Debugf("PUT %s:\n%s", addColabEndpoint, string(out))
	return err
}

// Add http request boilerplate and perform the request, checking for a successful response
func DoHttpThings(request *netHttp.Request, username, secret string) ([]byte, error) {
	message.Debugf("Performing %s http request to %#v", request.Method, request.URL)

	// Prep the request with boilerplate
	client := &netHttp.Client{Timeout: time.Second * 20}
	request.SetBasicAuth(username, secret)
	request.Header.Add("accept", "application/json")
	request.Header.Add("Content-Type", "application/json")

	// Perform the request and get the response
	response, err := client.Do(request)
	if err != nil {
		return []byte{}, err
	}
	responseBody, _ := io.ReadAll(response.Body)

	// If we get a 'bad' status code we will have no error, create a useful one to return
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err = fmt.Errorf("got status code of %d during http request with body of: %s", response.StatusCode, string(responseBody))
		return []byte{}, err
	}

	return responseBody, nil
}
