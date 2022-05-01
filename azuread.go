package orgmanager

import (
	"errors"
	"fmt"
	"strings"

	azidentity "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	abstractions "github.com/microsoft/kiota-abstractions-go"
	azurego "github.com/microsoft/kiota-authentication-azure-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	groupsitem "github.com/microsoftgraph/msgraph-sdk-go/groups/item"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	msgraph_errors "github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	usersitem "github.com/microsoftgraph/msgraph-sdk-go/users/item"

	"google.golang.org/protobuf/proto"
)

type AzureAD struct {
	client  *msgraphsdk.GraphServiceClient
	adapter abstractions.RequestAdapter
	config  *azureADConfig
}

func (a AzureAD) GetTargetSlug() string {
	return a.config.Slug
}

func (a AzureAD) GetPlatform() string {
	return a.config.Platform
}

type azureADConfig struct {
	Platform     string
	Slug         string
	TenantID     string
	ClientID     string
	ClientSecret string
	RootGroupID  string
}

func (a *AzureAD) InitFormUnmarshaler(unmarshaler func(any) error) (Target, error) {
	if err := unmarshaler(&a.config); err != nil {
		return nil, err
	}
	cred, err := azidentity.NewClientSecretCredential(a.config.TenantID, a.config.ClientID, a.config.ClientSecret, nil)
	if err != nil {
		return nil, fmt.Errorf("Error creating credentials: %v\n", err)
	}

	auth, err := azurego.NewAzureIdentityAuthenticationProviderWithScopes(cred, []string{"https://graph.microsoft.com/.default"})
	if err != nil {
		return nil, fmt.Errorf("Error authentication provider: %v\n", err)
	}
	a.adapter, err = msgraphsdk.NewGraphRequestAdapter(auth)
	if err != nil {
		return nil, fmt.Errorf("Error creating adapter: %v\n", err)
	}
	a.client = msgraphsdk.NewGraphServiceClient(a.adapter)
	return a, nil
}

func (d *AzureAD) RootDepartment() UnionDepartment {
	rootGroup, _ := d.client.GroupsById(d.config.RootGroupID).Get(nil)
	return &azureGroup{
		target: d,
		raw:    rootGroup,
	}
}

func (d AzureAD) LookupEntryUserByExternalIdentity(extID ExternalIdentity) (UnionUser, error) {
	requestParameters := &users.UsersRequestBuilderGetQueryParameters{
		Filter: proto.String(fmt.Sprintf("otherMails/any(id:id eq '%s')", extID)),
	}
	user, err := d.client.Users().Get(&users.UsersRequestBuilderGetOptions{
		QueryParameters: requestParameters,
	})
	if err != nil {
		return nil, err
	}
	if len(user.GetValue()) != 1 {
		return nil, errors.New("cannot identitify user")
	}
	return &azureADUser{
		target: &AzureAD{},
		raw:    user.GetValue()[0],
	}, nil
}

func (d AzureAD) LookupEntryDepartmentByExternalIdentity(extID ExternalIdentity) (UnionDepartment, error) {
	panic("not implemented") // TODO: Implement
}

type azureGroup struct {
	target *AzureAD
	raw    models.Groupable
}

func (g azureGroup) Name() (name string) {
	return *g.raw.GetDisplayName()
}

func (g azureGroup) DepartmentID() (departmentId string) {
	return *g.raw.GetId()
}

func (g azureGroup) SubDepartments() (departments []UnionDepartment) {
	groups, _ := g.target.client.GroupsById(*g.raw.GetId()).Members().Get(nil)
	for _, v := range groups.GetValue() {
		if *v.GetAdditionalData()["@odata.type"].(*string) == "#microsoft.graph.group" {
			group, _ := g.target.client.GroupsById(*v.GetId()).Get(nil)
			departments = append(departments, &azureGroup{
				target: g.target,
				raw:    group,
			})
		}
	}
	return departments
}

func (g *azureGroup) CreateSubDepartment(options DepartmentCreateOptions) (UnionDepartment, error) {
	newGroup := models.NewGroup()
	newGroup.SetDisplayName(proto.String(options.Name))
	newGroup.SetMailEnabled(proto.Bool(false))
	newGroup.SetMailNickname(proto.String("placeholder"))
	newGroup.SetSecurityEnabled(proto.Bool(true))
	newGroupable, err := g.target.client.Groups().Post(&groups.GroupsRequestBuilderPostOptions{
		Body: newGroup,
	})
	if err != nil {
		return nil, fmt.Errorf("Create group faild: %s", err)
	}
	opts := new(groups.GroupsRequestBuilderPostOptions)
	opts.Body = models.NewGroup()
	opts.Body.SetAdditionalData(map[string]any{
		"@odata.id": proto.String("https://graph.microsoft.com/v1.0/directoryObjects/" + *newGroupable.GetId()),
	})
	opts.Headers = make(map[string]string)
	opts.Headers["Content-Type"] = "application/json"
	requestBuilder := groups.NewGroupsRequestBuilder("https://graph.microsoft.com/v1.0/groups/"+*g.raw.GetId()+"/members/$ref", g.target.adapter)
	err = azureHackPost(requestBuilder, g.target.adapter, opts)
	if err != nil {
		err = fmt.Errorf("Link group membership faild: %s", err)
		if oDataError, ok := err.(*msgraph_errors.ODataError); ok {
			fmt.Println(oDataError.GetError())
			fmt.Println(oDataError.GetError().GetCode())
		}
	}
	return &azureGroup{
		target: g.target,
		raw:    newGroupable,
	}, err
}

//case the func on doc is unavailable
func azureHackPost(m *groups.GroupsRequestBuilder, requestAdapter abstractions.RequestAdapter, options *groups.GroupsRequestBuilderPostOptions) error {
	requestInfo, err := m.CreatePostRequestInformation(options)
	if err != nil {
		return err
	}
	errorMapping := abstractions.ErrorMappings{
		"4XX": odataerrors.CreateODataErrorFromDiscriminatorValue,
		"5XX": odataerrors.CreateODataErrorFromDiscriminatorValue,
	}
	_, err = requestAdapter.SendAsync(requestInfo, models.CreateGroupFromDiscriminatorValue, nil, errorMapping)
	if err != nil {
		return err
	}
	return nil
}

func (g *azureGroup) Users() (users []UnionUser) {
	groups, _ := g.target.client.GroupsById(*g.raw.GetId()).Members().Get(nil)
	for _, v := range groups.GetValue() {
		if *v.GetAdditionalData()["@odata.type"].(*string) == "#microsoft.graph.user" {
			user, _ := g.target.client.UsersById(*v.GetId()).Get(nil)
			users = append(users, &azureADUser{
				target: g.target,
				raw:    user,
			})
		}
	}
	return users
}

func (u *azureGroup) GetExternalIdentities() []ExternalIdentity {
	desc := ""
	if u.raw.GetDescription() != nil {
		desc = *u.raw.GetDescription()
	}
	return ExternalIdentitiesFromStringList(strings.Split(desc, ","))
}

func (u azureGroup) SetExternalIdentities(extIDs []ExternalIdentity) error {
	extIDStrList := make([]string, 0)
	for _, extID := range extIDs {
		extIDStrList = append(extIDStrList, string(extID))
	}
	newGroup := models.NewGroup()
	newGroup.SetDescription(proto.String(strings.Join(extIDStrList, ",")))
	return u.target.client.GroupsById(*u.raw.GetId()).Patch(&groupsitem.GroupItemRequestBuilderPatchOptions{
		Body: newGroup,
	})
}

type azureADUser struct {
	target *AzureAD
	raw    models.Userable
}

func (u azureADUser) ExternalIdentity() ExternalIdentity {
	return ExternalIdentity(fmt.Sprintf("ei.user.%s@%s.%s", *u.raw.GetId(), u.target.config.Slug, u.target.config.Platform))
}

func (u azureADUser) UserId() string {
	return *u.raw.GetId()
}

func (u azureADUser) UserName() string {
	return *u.raw.GetDisplayName()
}

func (u azureADUser) UserEmail() string {
	return *u.raw.GetMail()
}

func (u azureADUser) GetExternalIdentities() []ExternalIdentity {
	return ExternalIdentitiesFromStringList(u.raw.GetOtherMails())
}

func (u azureADUser) SetExternalIdentities(extIDs []ExternalIdentity) error {
	newOtherMails := make([]string, 0)
	for _, mail := range u.raw.GetOtherMails() {
		if _, err := ExternalIdentityParseString(mail); err != nil {
			newOtherMails = append(newOtherMails, mail)
		}
	}
	for _, extID := range extIDs {
		newOtherMails = append(newOtherMails, string(extID))
	}
	newUser := models.NewUser()
	newUser.SetOtherMails(append(newOtherMails))
	return u.target.client.UsersById(*u.raw.GetId()).Patch(&usersitem.UserItemRequestBuilderPatchOptions{
		Body: newUser,
	})
}
