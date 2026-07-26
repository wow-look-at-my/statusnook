package main

import (
	"fmt"
	"net/mail"
	"strings"
)

func (a *configApplier) applyServices() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
	}

	existingServiceSlugs := map[string]int{}

	services, err := listServices(tx)
	if err != nil {
		return fmt.Errorf("applyConfig.listServices: %w", err)
	}

	for _, v := range services {
		if _, ok := cfg.Services[v.Slug]; !ok {
			err := deleteServiceByID(tx, v.ID)
			if err != nil {
				return fmt.Errorf("applyConfig.deleteServiceByID: %w", err)
			}
			continue
		}

		existingServiceSlugs[v.Slug] = v.ID
	}

	processedServices := map[string]bool{}

	for slug, v := range cfg.Services {
		processedServices[slug] = true

		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("services", slug)
			continue
		}

		if v.Name == "" {
			msgs = append(msgs, "services."+slug+": name is required")
		}

		if _, ok := existingServiceSlugs[slug]; ok {
			err = editService(tx, existingServiceSlugs[slug], v.Name, v.Description)
			if err != nil {
				return fmt.Errorf("applyConfig.editService: %w", err)
			}
		} else {
			err = createService(tx, slug, v.Name, v.Description)
			if err != nil {
				return fmt.Errorf("applyConfig.createService: %w", err)
			}
		}
	}

	return nil
}

func (a *configApplier) applyMailGroups() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
	}

	existingMailGroupSlugs := map[string]int{}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		return fmt.Errorf("applyConfig.listMailGroups2: %w", err)
	}

	for _, v := range mailGroups {
		if _, ok := cfg.MailGroups[v.Slug]; !ok {
			err := deleteMailGroupByID(tx, v.ID)
			if err != nil {
				return fmt.Errorf("applyConfig.deleteMailGroupByID: %w", err)
			}
			continue
		}
		existingMailGroupSlugs[v.Slug] = v.ID
	}

	for slug, v := range cfg.MailGroups {
		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("mail-groups", slug)
			continue
		}

		if v.Name == "" {
			msgs = append(msgs, "mail-groups."+slug+": name is required")
		}

		uniqueMembers := map[string]bool{}

		for _, v := range v.Members {
			email, err := mail.ParseAddress(v)
			if err != nil {
				msgs = append(msgs, "mail-groups."+slug+": email is invalid \""+v+"\"")
				continue
			}

			if _, ok := uniqueMembers[strings.ToLower(email.String())]; ok {
				msgs = append(msgs, "mail-groups."+slug+": member is duplicated \""+v+"\"")
			}

			uniqueMembers[strings.ToLower(email.String())] = true
		}

		if _, ok := existingMailGroupSlugs[slug]; ok {
			err := updateMailGroup(tx, existingMailGroupSlugs[slug], v.Name, v.Description)
			if err != nil {
				return fmt.Errorf("applyConfig.updateMailGroup: %w", err)
			}
			err = updateMailGroupMembers(tx, existingMailGroupSlugs[slug], v.Members)
			if err != nil {
				return fmt.Errorf("applyConfig.updateMailGroupMembersUpdate: %w", err)
			}
		} else {
			id, err := createMailGroup(tx, slug, v.Name, v.Description)
			if err != nil {
				return fmt.Errorf("applyConfig.createMailGroup: %w", err)
			}

			err = updateMailGroupMembers(tx, id, v.Members)
			if err != nil {
				return fmt.Errorf("applyConfig.updateMailGroupMembersCreate: %w", err)
			}
		}
	}

	return nil
}
