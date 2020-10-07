package main

import (
	"encoding/csv"
	"fmt"
	"github.com/apex/log"
	"github.com/apex/log/handlers/cli"
	aws_ssmhelpers "github.com/disneystreaming/go-ssmhelpers/aws"
	"github.com/fatih/color"
	"github.com/jckuester/awsls/aws"
	"github.com/jckuester/awsls/internal"
	"github.com/jckuester/awsls/resource"
	"github.com/jckuester/awsls/util"
	"github.com/jckuester/terradozer/pkg/provider"
	flag "github.com/spf13/pflag"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	os.Exit(mainExitCode())
}

// this will fetch and print all the resources specified
func mainExitCode() int {

	var logDebug bool
	var allProfilesFlag bool
	var profiles internal.CommaSeparatedListFlag
	var regions internal.CommaSeparatedListFlag
	//var attributes internal.CommaSeparatedListFlag
	var version bool

	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	flags.Usage = func() {
		printHelp(flags)
	}

	flags.BoolVar(&logDebug, "debug", false, "Enable debug logging")
	flags.VarP(&profiles, "profiles", "p", "Comma-separated list of named AWS profiles for accounts to list resources in")
	flags.BoolVar(&allProfilesFlag, "all-profiles", false, "List resources for all profiles in ~/.aws/config")
	flags.VarP(&regions, "regions", "r", "Comma-separated list of regions to list resources in")
	flags.BoolVar(&version, "version", false, "Show application version")

	_ = flags.Parse(os.Args[1:])

	fmt.Println()
	defer fmt.Println()

	log.SetHandler(cli.Default)

	if logDebug {
		log.SetLevel(log.DebugLevel)
	}

	if version {
		fmt.Println(internal.BuildVersionString())
		return 0
	}

	if profiles != nil && allProfilesFlag == true {
		fmt.Fprint(os.Stderr, color.RedString("Error:️ --profiles and --all-profiles flag cannot be used together\n"))
		printHelp(flags)

		return 1
	}

	if profiles == nil && allProfilesFlag == false {
		env, ok := os.LookupEnv("AWS_PROFILE")
		if ok {
			profiles = []string{env}
		}
	}

	if allProfilesFlag {
		var awsConfigPath []string
		awsConfigFileEnv, ok := os.LookupEnv("AWS_CONFIG_FILE")
		if ok {
			awsConfigPath = []string{awsConfigFileEnv}
		}

		profilesFromConfig, err := aws_ssmhelpers.GetAWSProfiles(awsConfigPath...)
		if err != nil {
			fmt.Fprint(os.Stderr, color.RedString("Error: failed to load all profiles: %s\n", err))
			return 1
		}

		if profilesFromConfig == nil {
			fmt.Fprint(os.Stderr, color.RedString("Error: no profiles found in ~/.aws/config\n"))
			return 1
		}

		profiles = profilesFromConfig
	}
	clients, err := util.NewAWSClientPool(profiles, regions)
	if err != nil {
		fmt.Fprint(os.Stderr, color.RedString("\nError: %s\n", err))

		return 1
	}
	clientKeys := make([]util.AWSClientKey, 0, len(clients))
	for k := range clients {
		clientKeys = append(clientKeys, k)
	}
	// suppress provider debug and info logs
	log.SetLevel(log.ErrorLevel)
	if logDebug {
		log.SetLevel(log.DebugLevel)
	}
	// initialize a Terraform AWS provider for each AWS client with a matching config
	providers, err := util.NewProviderPool(clientKeys, "2.68.0", "~/.awsls", 10*time.Second)
	if err != nil {
		fmt.Fprint(os.Stderr, color.RedString("\nError: %s\n", err))

		return 1
	}
	defer func() {
		for _, p := range providers {
			_ = p.Close()
		}
	}()

	attributes := []string{"private_ip", "public_ip", "tags"}
	printResource("aws_instance", attributes, clients, providers)
	return 0
}

func printResource(resourceTypePattern string, attributes []string, clients map[util.AWSClientKey]aws.Client, providers map[util.AWSClientKey]provider.TerraformProvider) {
	matchedTypes, err := resource.MatchSupportedTypes(resourceTypePattern)
	if err != nil {
		fmt.Fprint(os.Stderr, color.RedString("Error: invalid glob pattern: %s\n", resourceTypePattern))
		panic(err)
	}

	if len(matchedTypes) == 0 {
		fmt.Fprint(os.Stderr, color.RedString("Error: no resource type found: %s\n", resourceTypePattern))
	}

	for _, rType := range matchedTypes {
		var resources []aws.Resource
		var hasAttrs map[string]bool

		for key, client := range clients {
			err := client.SetAccountID()
			if err != nil {
				fmt.Fprint(os.Stderr, color.RedString("Error %s: %s\n", rType, err))
				panic(err)
			}

			res, err := aws.ListResourcesByType(&client, rType)
			if err != nil {
				fmt.Fprint(os.Stderr, color.RedString("Error %s: %s\n", rType, err))
				continue
			}

			terraformProvider := providers[key]

			hasAttrs, err = resource.HasAttributes(attributes, rType, &terraformProvider)
			if err != nil {
				fmt.Fprint(os.Stderr, color.RedString("Error: failed to check if resource type has attribute: "+
					"%s\n", err))

				continue
			}

			if len(hasAttrs) > 0 {
				// for performance reasons:
				// only fetch state if some attributes need to be displayed for this resource type
				res = resource.GetStates(res, providers)
			}

			resources = append(resources, res...)
		}

		if len(resources) == 0 {
			continue
		}
		printResourcesCsv(resourceTypePattern, resources, hasAttrs, attributes)
	}
}

// print resources in csv format, and save it into the aws-resource folder
func printResourcesCsv(resourceTypePattern string, resources []aws.Resource, hasAttrs map[string]bool, attributes []string) {
	filePath := filepath.Join("aws-resources/", resourceTypePattern+".csv")
	err := os.MkdirAll("aws-resources/", os.ModePerm)
	if err != nil {
		panic(err)
	}
	csvFile, err := os.Create(filePath)
	if err != nil {
		panic(err)
	}
	defer csvFile.Close()
	w := csv.NewWriter(csvFile)

	printHeaderCsv(w, attributes)

	for _, r := range resources {
		resourceItem := []string{r.Type, r.ID}
		if r.CreatedAt != nil {
			resourceItem = append(resourceItem, r.CreatedAt.Format("2006-01-02 15:04:05"))
		} else {
			resourceItem = append(resourceItem, "")
		}
		for _, attr := range attributes {
			v := "N/A"
			_, ok := hasAttrs[attr]
			if ok {
				var err error
				v, err = resource.GetAttribute(attr, &r)
				if err != nil {
					log.WithFields(log.Fields{
						"type": r.Type,
						"id":   r.ID}).WithError(err).Debug("failed to get attribute")
					v = "error"
				}
			}
			resourceItem = append(resourceItem, v)
		}
		err := w.Write(resourceItem)
		if err != nil {
			panic(err)
		}
	}
	w.Flush()
	_, _ = fmt.Printf("printed csv file into %s \n", csvFile.Name())
}

// print csv header with fixed type and attributes
func printHeaderCsv(w *csv.Writer, attributes []string) {
	header := []string{"TYPE", "ID", "CREATED"}
	for _, attribute := range attributes {
		header = append(header, attribute)
	}
	err := w.Write(header)
	if err != nil {
		panic(err)
	}
}

func printHelp(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, "\n"+strings.TrimSpace(help)+"\n")
	fs.PrintDefaults()
}

const help = `
awsls - list AWS resources.

USAGE:
  $ awsls [flags] <resource_type glob pattern>

FLAGS:
`
