package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aquasecurity/trivy/pkg/flag"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aquasecurity/trivy/pkg/cloud/aws/scanner"
	"github.com/aquasecurity/trivy/pkg/cloud/report"

	"golang.org/x/xerrors"

	cmd "github.com/aquasecurity/trivy/pkg/commands/artifact"
	"github.com/aquasecurity/trivy/pkg/log"

	awsScanner "github.com/aquasecurity/defsec/pkg/scanners/cloud/aws"
)

const provider = "aws"

func getAccountIDAndRegion(ctx context.Context, region string) (string, string, error) {
	log.Logger.Debug("Looking for AWS credentials provider...")

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", "", err
	}
	if region != "" {
		cfg.Region = region
	}

	svc := sts.NewFromConfig(cfg)

	log.Logger.Debug("Looking up AWS caller identity...")
	result, err := svc.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", fmt.Errorf("failed to discover AWS caller identity: %w", err)
	}
	if result.Account == nil {
		return "", "", fmt.Errorf("missing account id for aws account")
	}
	log.Logger.Debugf("Verified AWS credentials for account %s!", *result.Account)
	return *result.Account, cfg.Region, nil
}

func Run(ctx context.Context, opt flag.Options) error {

	ctx, cancel := context.WithTimeout(ctx, opt.GlobalOptions.Timeout)
	defer cancel()

	if err := log.InitLogger(opt.Debug, false); err != nil {
		return fmt.Errorf("logger error: %w", err)
	}

	var err error
	defer func() {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Logger.Warn("Increase --timeout value")
		}
	}()

	reportOptions := report.Option{
		Format:      opt.Format,
		Output:      opt.Output,
		Severities:  opt.Severities,
		ReportLevel: report.LevelService,
	}
	if len(opt.Services) == 1 {
		reportOptions.ReportLevel = report.LevelResource
		reportOptions.Service = opt.Services[0]
		if opt.ARN != "" {
			reportOptions.ReportLevel = report.LevelResult
			reportOptions.ARN = opt.ARN
		}
	} else if opt.ARN != "" {
		return fmt.Errorf("you must specify the single --service which the --arn relates to")
	}

	accountID := opt.Account
	region := opt.Region
	if accountID == "" || region == "" {
		accountID, region, err = getAccountIDAndRegion(ctx, opt.Region)
		if err != nil {
			return err
		}
	}

	allSelectedServices := opt.Services

	if len(allSelectedServices) == 0 {
		log.Logger.Debug("No service(s) specified, scanning all services...")
		allSelectedServices = awsScanner.AllSupportedServices()
	} else {
		log.Logger.Debugf("Specific services were requested: [%s]...", strings.Join(allSelectedServices, ", "))
		for _, service := range allSelectedServices {
			var found bool
			supported := awsScanner.AllSupportedServices()
			for _, allowed := range supported {
				if allowed == service {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("service '%s' is not currently supported - supported services are: %s", service, strings.Join(supported, ", "))
			}
		}
	}

	var cached *report.Report

	if !opt.UpdateCache {
		log.Logger.Debugf("Attempting to load results from cache (%s)...", opt.CacheDir)
		cached, err = report.LoadReport(opt.CacheDir, provider, accountID, region, nil)
		if err != nil {
			if err != report.ErrCacheNotFound {
				return err
			}
			log.Logger.Debug("Cached results not found.")
		}
	}

	var remaining []string
	var cachedServices []string
	for _, service := range allSelectedServices {
		if cached != nil {
			var inCache bool
			for _, cacheSvc := range cached.ServicesInScope {
				if cacheSvc == service {
					log.Logger.Debugf("Results for service '%s' found in cache.", service)
					inCache = true
					break
				}
			}
			if inCache {
				cachedServices = append(cachedServices, service)
				continue
			}
		}
		remaining = append(remaining, service)
	}

	var r *report.Report

	// if there is anything we need that wasn't in the cache, scan it now
	if len(remaining) > 0 {
		log.Logger.Debugf("Scanning the following services using the AWS API: [%s]...", strings.Join(remaining, ", "))
		opt.Services = remaining
		results, err := scanner.NewScanner().Scan(ctx, opt)
		if err != nil {
			return xerrors.Errorf("aws scan error: %w", err)
		}
		r = report.New(accountID, region, results.GetFailed(), allSelectedServices)
	} else {
		log.Logger.Debug("No more services to scan - everything was found in the cache.")
		r = report.New(accountID, region, nil, allSelectedServices)
	}
	if cached != nil {
		log.Logger.Debug("Merging cached results...")
		r.Merge(cached, cached.ServicesInScope...)
		reportOptions.FromCache = true
	}

	if len(remaining) > 0 { // don't write cache if we didn't scan anything new
		log.Logger.Debugf("Writing results to cache for services [%s]...", strings.Join(r.ServicesInScope, ", "))
		if err := r.Save(opt.CacheDir, provider); err != nil {
			return err
		}
	}

	if len(allSelectedServices) > 0 {
		r = r.ForServices(allSelectedServices...)
	}

	log.Logger.Debug("Writing report to output...")
	if err := report.Write(r, reportOptions); err != nil {
		return fmt.Errorf("unable to write results: %w", err)
	}

	cmd.Exit(opt, r.Failed())
	return nil
}
