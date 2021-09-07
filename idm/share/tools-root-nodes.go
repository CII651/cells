package share

import (
	"context"
	"fmt"
	"github.com/pydio/cells/common/auth/claim"
	"github.com/pydio/cells/common/proto/idm"
	"strings"

	"github.com/pborman/uuid"
	"github.com/pydio/cells/common/proto/jobs"
	"github.com/pydio/cells/common/registry"

	"github.com/gosimple/slug"
	"github.com/micro/go-micro/errors"
	"go.uber.org/zap"

	"github.com/pydio/cells/common"
	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/micro"
	"github.com/pydio/cells/common/proto/rest"
	"github.com/pydio/cells/common/proto/tree"
	"github.com/pydio/cells/common/utils/permissions"
	"github.com/pydio/cells/common/views"
)

// LoadDetectedRootNodes find actual nodes in the tree, and enrich their metadata if they appear
// in many workspaces for the current user.
func LoadDetectedRootNodes(ctx context.Context, detectedRoots []string) (rootNodes map[string]*tree.Node) {

	rootNodes = make(map[string]*tree.Node)
	router := views.NewUuidRouter(views.RouterOptions{})
	metaClient := tree.NewNodeProviderClient(common.ServiceGrpcNamespace_+common.ServiceMeta, defaults.NewClient())
	eventFilter := views.NewRouterEventFilter(views.RouterOptions{AdminView: false})
	accessList, _ := permissions.AccessListFromContextClaims(ctx)
	for _, rootId := range detectedRoots {
		request := &tree.ReadNodeRequest{Node: &tree.Node{Uuid: rootId}}
		if resp, err := router.ReadNode(ctx, request); err == nil {
			node := resp.Node
			var multipleMeta []*tree.WorkspaceRelativePath
			for _, ws := range accessList.Workspaces {
				if filtered, ok := eventFilter.WorkspaceCanSeeNode(ctx, accessList, ws, resp.Node); ok {
					multipleMeta = append(multipleMeta, &tree.WorkspaceRelativePath{
						WsLabel: ws.Label,
						WsUuid:  ws.UUID,
						WsSlug:  ws.Slug,
						Path:    filtered.Path,
					})
					node = filtered
				}
			}
			if len(multipleMeta) > 0 {
				node.AppearsIn = multipleMeta
			}
			if metaResp, e := metaClient.ReadNode(ctx, request); e == nil {
				var isRoomNode bool
				if mE := metaResp.GetNode().GetMeta("CellNode", &isRoomNode); mE == nil && isRoomNode {
					node.SetMeta("CellNode", true)
				}
			}
			rootNodes[node.GetUuid()] = node.WithoutReservedMetas()
		} else {
			log.Logger(ctx).Debug("Share Load - Ignoring Root Node, probably not synced yet", zap.String("nodeId", rootId), zap.Error(err))
		}
	}
	return

}

// ParseRootNodes reads the request property to either create a new node using the "rooms" Virtual node,
// or just verify that the root nodes are not empty.
func ParseRootNodes(ctx context.Context, shareRequest *rest.PutCellRequest) (error, *tree.Node, bool) {

	var createdNode *tree.Node
	router := views.NewStandardRouter(views.RouterOptions{})
	for i, n := range shareRequest.Room.RootNodes {
		r, e := router.ReadNode(ctx, &tree.ReadNodeRequest{Node: n})
		if e != nil {
			return e, nil, false
		}
		// If the virtual root is responded, it may miss the UUID ! Set up manually here
		if r.Node.Uuid == "" {
			r.Node.Uuid = n.Uuid
		}
		shareRequest.Room.RootNodes[i] = r.Node
	}
	if shareRequest.CreateEmptyRoot {

		manager := views.GetVirtualNodesManager()
		internalRouter := views.NewStandardRouter(views.RouterOptions{WatchRegistry: false, AdminView: true})
		if root, exists := manager.ByUuid("cells"); exists {
			parentNode, err := manager.ResolveInContext(ctx, root, internalRouter.GetClientsPool(), true)
			if err != nil {
				return err, nil, false
			}
			index := 0
			labelSlug := slug.Make(shareRequest.Room.Label)
			baseSlug := labelSlug
			for {
				if existingResp, err := internalRouter.ReadNode(ctx, &tree.ReadNodeRequest{Node: &tree.Node{Path: parentNode.Path + "/" + labelSlug}}); err == nil && existingResp.Node != nil {
					index++
					labelSlug = fmt.Sprintf("%s-%v", baseSlug, index)
				} else {
					break
				}
			}
			createResp, err := internalRouter.CreateNode(ctx, &tree.CreateNodeRequest{
				Node: &tree.Node{Path: parentNode.Path + "/" + labelSlug},
			})
			if err != nil {
				log.Logger(ctx).Error("share/cells : create empty root", zap.Error(err))
				return err, nil, false
			}
			// Update node meta
			createResp.Node.SetMeta("CellNode", true)
			metaClient := tree.NewNodeReceiverClient(common.ServiceGrpcNamespace_+common.ServiceMeta, defaults.NewClient())
			metaClient.CreateNode(ctx, &tree.CreateNodeRequest{Node: createResp.Node})
			shareRequest.Room.RootNodes = append(shareRequest.Room.RootNodes, createResp.Node)
			createdNode = createResp.Node
		} else {
			return errors.InternalServerError(common.ServiceShare, "Wrong configuration, missing rooms virtual node"), nil, false
		}
	}
	if len(shareRequest.Room.RootNodes) == 0 {
		return errors.BadRequest(common.ServiceShare, "Wrong configuration, missing RootNodes in CellRequest"), nil, false
	}

	// First check of incoming ACLs
	var hasReadonly bool
	for _, root := range shareRequest.Room.RootNodes {
		if root.GetStringMeta(common.MetaFlagReadonly) != "" {
			hasReadonly = true
		}
	}
	if hasReadonly {
		for _, a := range shareRequest.Room.GetACLs() {
			for _, action := range a.GetActions() {
				if action.Name == permissions.AclWrite.Name {
					return errors.Forbidden(common.ServiceShare, "One of the resource you are sharing is readonly. You cannot assign write permission on this Cell."), nil, true
				}
			}
		}
	}
	log.Logger(ctx).Debug("ParseRootNodes", log.DangerouslyZapSmallSlice("r", shareRequest.Room.RootNodes), zap.Bool("readonly", hasReadonly))
	return nil, createdNode, hasReadonly

}

func DetectInheritedPolicy(ctx context.Context, roots []*tree.Node, loadedParents []*tree.WorkspaceRelativePath) (string, error) {

	var cellNode bool
	for _, r := range roots {
		if r.HasMetaKey("CellNode") {
			cellNode = true
			break
		}
	}
	if (cellNode) {
		// Check if there is a default policy set for cells using custom folders
		claims, ok := ctx.Value(claim.ContextKey).(claim.Claims)
		if !ok {
			return "", fmt.Errorf("cannot find claims in context")
		}
		roles := permissions.GetRoles(ctx, strings.Split(claims.Roles, ","))
		acls := permissions.GetACLsForRoles(ctx, roles, &idm.ACLAction{Name: "default-cells-policy"})
		var foundPolicy string
		for _, role := range roles {
			for _, acl := range acls {
				if acl.RoleID == role.Uuid && acl.Action.Name == "default-cells-policy" {
					foundPolicy = strings.TrimPrefix(strings.Trim(acl.Action.Value, `"`), "policy:")
				}
			}
		}
		if foundPolicy != "" {
			return foundPolicy, nil
		}
	}

	accessList, e := permissions.AccessListFromContextClaims(ctx)
	if e != nil {
		return "", e
	}
	if !accessList.HasPolicyBasedAcls() {
		return "", nil
	}
	var ww []*tree.WorkspaceRelativePath
	if loadedParents != nil {
		ww = loadedParents
	} else {
		rpw, e := RootsParentWorkspaces(ctx, roots)
		if e != nil {
			return "", e
		}
		ww = rpw
	}
	wsNodes := accessList.GetWorkspacesNodes()
	var parentPol string
	for _, w := range ww {
		if nn, ok := wsNodes[w.WsUuid]; ok {
			for _, b := range nn {
				if b.BitmaskFlag&permissions.FlagPolicy != 0 {
					for _, p := range b.PolicyIds {
						if strings.HasSuffix(p, "-ro") || strings.HasSuffix(p, "-rw") || strings.HasSuffix(p, "-wo") {
							continue
						}
						parentPol = p
						break
					}
				}
			}
		}
	}
	return parentPol, nil
}

// DeleteRootNodeRecursively loads all children of a root node and delete them, including the
// .pydio hidden files when they are folders.
func DeleteRootNodeRecursively(ctx context.Context, ownerName string, roomNode *tree.Node) error {

	manager := views.GetVirtualNodesManager()
	router := views.NewStandardRouter(views.RouterOptions{WatchRegistry: false, AdminView: true})
	if root, exists := manager.ByUuid("cells"); exists {
		parentNode, err := manager.ResolveInContext(ctx, root, router.GetClientsPool(), true)
		if err != nil {
			return err
		}
		realNode := &tree.Node{Path: parentNode.Path + "/" + strings.TrimRight(roomNode.Path, "/")}
		// Now send deletion to scheduler
		cli := jobs.NewJobServiceClient(registry.GetClient(common.ServiceJobs))
		jobUuid := "cells-delete-" + uuid.New()
		job := &jobs.Job{
			ID:             jobUuid,
			Owner:          ownerName,
			Label:          "Deleting Cell specific data",
			MaxConcurrency: 1,
			AutoStart:      true,
			AutoClean:      true,
			Actions: []*jobs.Action{
				{
					ID:         "actions.tree.delete",
					Parameters: map[string]string{},
					NodesSelector: &jobs.NodesSelector{
						Pathes: []string{realNode.Path},
					},
				},
			},
		}
		if _, er := cli.PutJob(ctx, &jobs.PutJobRequest{Job: job}); er != nil {
			return er
		}
	}
	return nil
}

// CheckLinkRootNodes loads the root nodes and check if one of the is readonly. If so, check that
// link permissions do not try to set the Upload mode.
func CheckLinkRootNodes(ctx context.Context, link *rest.ShareLink) (workspaces []*tree.WorkspaceRelativePath, files, folders bool, e error) {

	router := views.NewUuidRouter(views.RouterOptions{})
	var hasReadonly bool
	for i, r := range link.RootNodes {
		resp, er := router.ReadNode(ctx, &tree.ReadNodeRequest{Node: r})
		if er != nil {
			e = er
			return
		}
		if resp.Node == nil {
			e = errors.NotFound(common.ServiceShare, "cannot find root node")
			return
		}
		link.RootNodes[i] = resp.Node
		if resp.Node.GetStringMeta(common.MetaFlagReadonly) != "" {
			hasReadonly = true
		}
		workspaces = append(workspaces, resp.Node.AppearsIn...)
		if resp.Node.IsLeaf() {
			files = true
		} else {
			folders = true
		}
	}
	if hasReadonly {
		for _, p := range link.Permissions {
			if p == rest.ShareLinkAccessType_Upload {
				e = errors.Forbidden(common.ServiceShare, "This resource is not writeable, you are not allowed to set this permission.")
				return
			}
		}
	}
	return

}

func RootsParentWorkspaces(ctx context.Context, rr []*tree.Node) (ww []*tree.WorkspaceRelativePath, e error) {
	router := views.NewUuidRouter(views.RouterOptions{})
	for _, r := range rr {
		resp, er := router.ReadNode(ctx, &tree.ReadNodeRequest{Node: &tree.Node{Uuid: r.Uuid}})
		if er != nil {
			e = er
			return
		}
		if resp.Node == nil {
			e = errors.NotFound(common.ServiceShare, "cannot find root node")
			return
		}
		ww = append(ww, resp.Node.AppearsIn...)
	}
	return
}