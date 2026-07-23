package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arcbjorn/odin/model"
	"github.com/arcbjorn/odin/profile"
)

// cmdAuth runs the configured provider login and stores the token 0600.
func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	common := bindCommon(fs)
	providerName := fs.String("provider", "", "provider name from config.toml")
	accountName := fs.String("account", "", "named provider account")
	showStatus := fs.Bool("status", false, "show token expiry instead of logging in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}
	if err := p.EnsureDirs(); err != nil {
		return err
	}

	var target *profile.ProviderConfig
	for i := range p.Config.Providers {
		if p.Config.Providers[i].Name == *providerName {
			target = &p.Config.Providers[i]
			break
		}
	}
	if target == nil {
		var names []string
		for _, pc := range p.Config.Providers {
			if pc.OAuth || pc.Subscription != "" && pc.Subscription != "qwen" && pc.Subscription != "kimi" {
				names = append(names, pc.Name)
			}
		}
		return fmt.Errorf("--provider is required (login providers: %s)", strings.Join(names, ", "))
	}
	if target.Subscription == "qwen" || target.Subscription == "kimi" {
		return fmt.Errorf("provider %q uses a %s plan key from %s, not OAuth", target.Name, target.Subscription, target.APIKeyEnv)
	}
	if !target.OAuth && target.Subscription == "" {
		return fmt.Errorf("provider %q uses an api key (%s), not oauth", target.Name, target.APIKeyEnv)
	}
	authPath := p.AuthPath(target.Name)
	if len(target.Accounts) > 0 {
		if *accountName == "" {
			return fmt.Errorf("--account is required for provider %q (accounts: %s)", target.Name, strings.Join(target.Accounts, ", "))
		}
		configured := false
		for _, name := range target.Accounts {
			if name == *accountName {
				configured = true
				break
			}
		}
		if !configured {
			return fmt.Errorf("unknown account %q for provider %q (accounts: %s)", *accountName, target.Name, strings.Join(target.Accounts, ", "))
		}
		authPath = p.AccountAuthPath(target.Name, *accountName)
	} else if *accountName != "" {
		return fmt.Errorf("provider %q does not configure an account pool", target.Name)
	}

	var src *model.OAuthSource
	if target.Subscription != "" {
		subscriptionSource, err := model.NewSubscriptionSource(target.Subscription, authPath)
		if err != nil {
			return err
		}
		src = subscriptionSource.(*model.OAuthSource)
	} else {
		src = model.NewOAuthSource(model.OAuthConfig{
			Path: authPath, ClientID: target.ClientID,
			TokenURL: target.TokenURL, Scope: target.Scope,
		})
	}

	if *showStatus {
		expiresIn, lastRefresh, err := src.Status()
		if err != nil {
			return err
		}
		// Never print the token itself — this output gets pasted into issues.
		fmt.Printf("provider      %s\n", target.Name)
		if *accountName != "" {
			fmt.Printf("account       %s\n", *accountName)
		}
		fmt.Printf("credentials   %s\n", authPath)
		fmt.Printf("expires in    %s\n", expiresIn.Round(time.Second))
		fmt.Printf("last refresh  %s\n", formatTime(lastRefresh))
		if expiresIn <= 0 {
			fmt.Println("\nToken is expired; it will refresh on next use.")
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if target.Subscription != "" {
		fmt.Printf("Starting %s subscription login for %s", target.Subscription, target.Name)
		if *accountName != "" {
			fmt.Printf(" account %s", *accountName)
		}
		fmt.Println(".")
		reader := bufio.NewReader(os.Stdin)
		return model.LoginSubscription(ctx, target.Subscription, authPath, model.LoginUI{
			DeviceCode: func(userCode, verifyURL string) {
				fmt.Printf("\n  Open:  %s\n  Code:  %s\n\nWaiting for approval...\n", verifyURL, userCode)
			},
			AuthorizationCode: func(authorizeURL string) (string, error) {
				fmt.Printf("\n  Open: %s\n\nAfter authorizing, paste the code here: ", authorizeURL)
				code, err := reader.ReadString('\n')
				return strings.TrimSpace(code), err
			},
		})
	}

	fmt.Printf("Starting device login for %s.\n", target.Name)
	return src.DeviceLogin(ctx, target.DeviceURL, func(userCode, verifyURL string) {
		// Device flow, not a loopback redirect: the server has no browser and
		// no inbound port. Approve on a phone, the server polls.
		fmt.Printf("\n  Open:  %s\n  Code:  %s\n\nWaiting for approval...\n", verifyURL, userCode)
	})
}
