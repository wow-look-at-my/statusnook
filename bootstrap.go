package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"golang.org/x/crypto/bcrypt"
)

// applyEnvBootstrap reconciles the database with the environment on every
// start, so a container with STATUSNOOK_* set comes up ready to serve without
// anyone walking the setup wizard.
//
// Env values win: whatever the environment covers, the environment owns.
// see docs/deployment.md
func applyEnvBootstrap(tx *sql.Tx) error {
	if env.Name != "" {
		if err := updateMetaValue(tx, "name", env.Name); err != nil {
			return fmt.Errorf("applyEnvBootstrap.updateMetaValueName: %w", err)
		}
		metaName = env.Name
	}

	if err := applyEnvDomain(tx); err != nil {
		return err
	}

	if env.GitHub.Managed() {
		// The token is deliberately absent: it stays in memory only, so a
		// leaked database backup does not leak repository access.
		values := map[string]string{
			"configFileEnabled":   "true",
			"githubManagedConfig": "true",
			"githubRepoURL":       "https://github.com/" + env.GitHub.Repo,
			"githubConfigBranch":  env.GitHub.Branch,
			"githubConfigPath":    env.GitHub.Path,
			"githubConfigToken":   "",
		}

		for name, value := range values {
			if err := updateMetaValue(tx, name, value); err != nil {
				return fmt.Errorf("applyEnvBootstrap.updateMetaValue %s: %w", name, err)
			}
		}

		metaConfigFileEnabled = true
	}

	if env.AdminUsername != "" {
		if err := ensureEnvAdmin(tx); err != nil {
			return err
		}
	}

	return nil
}

// applyEnvDomain pins the public domain and the TLS mode.
func applyEnvDomain(tx *sql.Tx) error {
	if env.Domain == "" && env.TLS == "" {
		return nil
	}

	if env.Domain != "" {
		if err := updateMetaValue(tx, "domain", env.Domain); err != nil {
			return fmt.Errorf("applyEnvDomain.updateMetaValueDomain: %w", err)
		}
		metaDomain = env.Domain

		for _, name := range []string{"unconfirmedDomain", "unconfirmedDomainProblem"} {
			if err := updateMetaValue(tx, name, ""); err != nil {
				return fmt.Errorf("applyEnvDomain.updateMetaValue %s: %w", name, err)
			}
		}
		metaUnconfirmedDomain = ""
		metaUnconfirmedDomainProblem = ""
	}

	if env.TLS != "" {
		ssl := "false"
		if env.TLS == "auto" {
			ssl = "true"
		}

		if err := updateMetaValue(tx, "ssl", ssl); err != nil {
			return fmt.Errorf("applyEnvDomain.updateMetaValueSSL: %w", err)
		}
		metaSSL = ssl
	}

	return nil
}

// ensureEnvAdmin creates the admin account described by the environment, and
// resets its password when it drifts. Sessions are dropped on a reset so a
// stolen cookie does not outlive the password change.
func ensureEnvAdmin(tx *sql.Tx) error {
	hash, userID, err := getPasswordHash(tx, env.AdminUsername)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("ensureEnvAdmin.getPasswordHash: %w", err)
	}

	newHash, hashErr := bcrypt.GenerateFromPassword(
		[]byte(env.AdminPassword), bcrypt.DefaultCost,
	)
	if hashErr != nil {
		return fmt.Errorf("ensureEnvAdmin.GenerateFromPassword: %w", hashErr)
	}

	if errors.Is(err, sql.ErrNoRows) {
		if _, err := createUser(tx, env.AdminUsername, string(newHash)); err != nil {
			return fmt.Errorf("ensureEnvAdmin.createUser: %w", err)
		}
		log.Printf("created admin user %q from the environment", env.AdminUsername)

		return markSetupDone(tx)
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(env.AdminPassword)) == nil {
		return markSetupDone(tx)
	}

	if err := editUser(tx, userID, env.AdminUsername, string(newHash)); err != nil {
		return fmt.Errorf("ensureEnvAdmin.editUser: %w", err)
	}

	if err := deleteAllSessionsByUserID(tx, userID); err != nil {
		return fmt.Errorf("ensureEnvAdmin.deleteAllSessionsByUserID: %w", err)
	}

	log.Printf("reset password for admin user %q from the environment", env.AdminUsername)

	return markSetupDone(tx)
}

// markSetupDone retires the setup wizard. The environment provided everything
// the wizard would have asked for.
func markSetupDone(tx *sql.Tx) error {
	if metaSetup == "done" {
		return nil
	}

	if metaName == "" {
		if err := updateMetaValue(tx, "name", "Statusnook"); err != nil {
			return fmt.Errorf("markSetupDone.updateMetaValueName: %w", err)
		}
		metaName = "Statusnook"
	}

	if err := updateMetaValue(tx, "setup", "done"); err != nil {
		return fmt.Errorf("markSetupDone.updateMetaValueSetup: %w", err)
	}
	metaSetup = "done"

	return nil
}
