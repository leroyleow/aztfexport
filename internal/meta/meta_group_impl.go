package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/magodo/armid"
	"github.com/magodo/azlist/azlist"

	"github.com/Azure/aztfy/internal/resmap"
	"github.com/Azure/aztfy/internal/resourceset"
	"github.com/Azure/aztfy/internal/tfaddr"

	"github.com/Azure/aztfy/internal/config"
)

var _ GroupMeta = &MetaGroupImpl{}

type MetaGroupImpl struct {
	Meta

	// Only non empty when in resource group mode
	resourceGroup string

	argPredicate   string
	recursiveQuery bool

	// Key is azure resource id; Value is terraform resource addr.
	// For azure resources not in this mapping, they are all initialized as to skip.
	resourceMapping resmap.ResourceMapping

	resourceNamePrefix string
	resourceNameSuffix string
}

func newGroupMetaImpl(cfg config.GroupConfig) (GroupMeta, error) {
	baseMeta, err := NewMeta(cfg.CommonConfig)
	if err != nil {
		return nil, err
	}

	argPredicate := cfg.ARGPredicate
	if argPredicate == "" {
		argPredicate = fmt.Sprintf("resourceGroup =~ %q", cfg.ResourceGroupName)
	}

	meta := &MetaGroupImpl{
		Meta:            *baseMeta,
		resourceGroup:   cfg.ResourceGroupName,
		argPredicate:    argPredicate,
		recursiveQuery:  cfg.RecursiveQuery,
		resourceMapping: cfg.ResourceMapping,
	}

	if pos := strings.LastIndex(cfg.ResourceNamePattern, "*"); pos != -1 {
		meta.resourceNamePrefix, meta.resourceNameSuffix = cfg.ResourceNamePattern[:pos], cfg.ResourceNamePattern[pos+1:]
	} else {
		meta.resourceNamePrefix = cfg.ResourceNamePattern
	}

	return meta, nil
}

func (meta MetaGroupImpl) ScopeName() string {
	if meta.resourceGroup != "" {
		return meta.resourceGroup
	}
	msg := meta.argPredicate
	if meta.recursiveQuery {
		msg += " (recursive)"
	}
	return msg
}

func (meta *MetaGroupImpl) ListResource() (ImportList, error) {
	ctx := context.TODO()

	rset, err := meta.queryResourceSet(ctx)
	if err != nil {
		return nil, err
	}
	if err := rset.PopulateResource(); err != nil {
		return nil, fmt.Errorf("tweaking single resources in the azure resource set: %v", err)
	}
	if err := rset.ReduceResource(); err != nil {
		return nil, fmt.Errorf("tweaking across resources in the azure resource set: %v", err)
	}

	rl := rset.ToTFResources()

	// Some RP will flip the casing on fields like resource group name, causing the TF ID is inconsistent on casing.
	// We shall check the existance of the resource id case insensitively.
	type MapInfo struct {
		id   string
		addr tfaddr.TFAddr
	}
	caseInsensitiveMapping := map[string]MapInfo{}
	for k, v := range meta.resourceMapping {
		caseInsensitiveMapping[strings.ToUpper(k)] = MapInfo{
			id:   k,
			addr: v,
		}
	}

	var l ImportList
	for i, res := range rl {
		item := ImportItem{
			AzureResourceID: res.AzureId,
			TFResourceId:    res.TFId,
			TFAddr: tfaddr.TFAddr{
				Type: "",
				Name: fmt.Sprintf("%s%d%s", meta.resourceNamePrefix, i, meta.resourceNameSuffix),
			},
		}
		if res.TFType != "" {
			item.Recommendations = []string{res.TFType}
		}

		if len(caseInsensitiveMapping) != 0 {
			// TODO: There is a potential issue here that the more than one Azure resources might have the same TF resource id (e.g. a parent resource and its singleton child resource).
			// 		 Ideally, we should refactor the resource mapping file to make the Azure resource id as key, and record the TF id and TF address (type + name) as its value.
			if info, ok := caseInsensitiveMapping[strings.ToUpper(res.TFId)]; ok {
				item.TFResourceId = info.id
				item.TFAddr = info.addr
			}
		} else {
			// Only auto deduce the TF resource type from recommendations when there is no resource mapping file specified.
			if res.TFType != "" {
				item.TFAddr.Type = res.TFType
				item.IsRecommended = true
			}
		}
		l = append(l, item)
	}
	return l, nil
}

func (meta MetaGroupImpl) ExportResourceMapping(l ImportList) error {
	m := resmap.ResourceMapping{}
	for _, item := range l {
		m[item.TFResourceId] = item.TFAddr
	}
	output := filepath.Join(meta.Workspace(), ResourceMappingFileName)
	b, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return fmt.Errorf("JSON marshalling the resource mapping: %v", err)
	}
	if err := os.WriteFile(output, b, 0644); err != nil {
		return fmt.Errorf("writing the resource mapping to %s: %v", output, err)
	}
	return nil
}

func ptr[T any](v T) *T {
	return &v
}

func (meta MetaGroupImpl) queryResourceSet(ctx context.Context) (*resourceset.AzureResourceSet, error) {
	result, err := azlist.List(ctx, meta.subscriptionId, meta.argPredicate, &azlist.Option{Parallelism: meta.parallelism, Recursive: meta.recursiveQuery})
	if err != nil {
		return nil, fmt.Errorf("listing resource set: %v", err)
	}

	var rl []resourceset.AzureResource
	for _, res := range result.Resources {
		res := resourceset.AzureResource{
			Id:         res.Id,
			Properties: res.Properties,
		}
		rl = append(rl, res)
	}

	// Especially, if this is for resource group, adding the resoruce group itself to the resource set
	if meta.resourceGroup != "" {
		rl = append(rl, resourceset.AzureResource{Id: &armid.ResourceGroup{
			SubscriptionId: meta.subscriptionId,
			Name:           meta.resourceGroup,
		}})
	}

	return &resourceset.AzureResourceSet{Resources: rl}, nil
}