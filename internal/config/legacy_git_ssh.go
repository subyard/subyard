package config

import "errors"

// LegacyGitSSHPort permits reading old static source without granting new Git
// authoring authority. Migration pins its effective value locally first.
func LegacyGitSSHPort(path string, content []byte) error {
	assignments, err := ParsePersistentAssignments(path, content)
	if err != nil {
		return err
	}
	for _, assignment := range assignments {
		if assignment.Name == "SSH_PORT" && (!assignment.Direct || assignment.Dynamic) {
			return errors.New("legacy Git SSH_PORT must be a literal direct assignment; move it to local settings before retrying")
		}
	}
	return nil
}
