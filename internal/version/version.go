// Package version holds the application version and the GitHub repository
// releases are published to. Bump Version together with [app] version in
// viva.toml when cutting a release; a test keeps the two in step.
package version

// Version is the running build's version, without the leading "v".
const Version = "1.0.1"

// Repo is the GitHub repository ("owner/name") whose releases the update
// check reads.
const Repo = "972579/open-image-mcp"

// RepoURL is the repository's web page.
const RepoURL = "https://github.com/" + Repo
