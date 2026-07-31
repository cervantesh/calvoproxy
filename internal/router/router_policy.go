package router

import (
	"time"

	"github.com/cervantesh/calvoproxy/internal/router/policyvocab"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

const (
	defaultPolicyTimeout = 45 * time.Second

	capChatCompletion    cervorules.Operation = policyvocab.OperationChatCompletion
	capToolOrchestration cervorules.Operation = policyvocab.OperationToolOrchestration
	capTelegramWebhook   cervorules.Operation = policyvocab.OperationTelegramWebhook
	capSecretLookup      cervorules.Operation = policyvocab.OperationSecretLookup
	capPlanning          cervorules.Operation = policyvocab.OperationPlanning
	capMediaRequest      cervorules.Operation = policyvocab.OperationMediaRequest
	capEmbedding         cervorules.Operation = policyvocab.OperationEmbedding

	serviceCervoCore     cervorules.Target = policyvocab.TargetCervoCore
	serviceCervoBridge   cervorules.Target = policyvocab.TargetCervoBridge
	serviceCervoVaultAPI cervorules.Target = policyvocab.TargetCervoVaultAPI
	serviceCrashitoBrain cervorules.Target = policyvocab.TargetCrashitoBrain
	serviceCervoMedia    cervorules.Target = policyvocab.TargetCervoMedia

	providerOpenRouter cervorules.Executor = policyvocab.ExecutorOpenRouter
	providerOpenAI     cervorules.Executor = policyvocab.ExecutorOpenAI
	providerAnthropic  cervorules.Executor = policyvocab.ExecutorAnthropic
	providerOllama     cervorules.Executor = policyvocab.ExecutorOllama
	providerLocal      cervorules.Executor = policyvocab.ExecutorLocal
)

type envRouteOverride struct {
	Env       string
	Operation cervorules.Operation
}

var envRouteOverrides = []envRouteOverride{
	{Env: "PROXY_PLANNING_SERVICE", Operation: capPlanning},
	{Env: "PROXY_MEDIA_SERVICE", Operation: capMediaRequest},
}

var proxyHTTPClassificationOptions = httpClassificationOptions{
	Headers: httpHeaderOptions{
		RequestID: []string{headerRequestID, "X-Correlation-Id"},
		User:      []string{headerUser, "X-User", "X-OpenClaw-User"},
		Channel:   []string{headerChannel, "X-Channel", "X-OpenClaw-Channel"},
		Risk:      []string{headerRisk},
		Operation: []string{headerCapability},
	},
	PathOperations: []pathOperation{
		{Methods: []string{"POST"}, Prefix: "/v1/chat/completions", Operation: capChatCompletion},
		{Methods: []string{"POST"}, Prefix: "/v1/messages", Operation: capChatCompletion},
		{Methods: []string{"POST"}, Prefix: "/v1/embeddings", Operation: capEmbedding},
		{Prefix: "/telegram", Operation: capTelegramWebhook},
		{Regex: `/(secret|vault)(/|$)`, Operation: capSecretLookup},
		{Regex: `/(media|images|audio|video)(/|$)`, Operation: capMediaRequest},
		{Regex: `/(models|plan|planning)(/|$)`, Operation: capPlanning},
	},
	ChannelOperations: map[string]cervorules.Operation{
		"telegram": capTelegramWebhook,
	},
}

var proxyHTTPClassifier = mustNewProxyHTTPClassifier(proxyHTTPClassificationOptions)

func mustNewProxyHTTPClassifier(options httpClassificationOptions) *httpClassifier {
	classifier, err := newHTTPClassifier(options)
	if err != nil {
		panic(err)
	}
	return classifier
}
