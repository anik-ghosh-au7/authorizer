package resolvers

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/authorizerdev/authorizer/server/constants"
	"github.com/authorizerdev/authorizer/server/cookie"
	"github.com/authorizerdev/authorizer/server/crypto"
	"github.com/authorizerdev/authorizer/server/db"
	"github.com/authorizerdev/authorizer/server/db/models"
	"github.com/authorizerdev/authorizer/server/email"
	"github.com/authorizerdev/authorizer/server/envstore"
	"github.com/authorizerdev/authorizer/server/graph/model"
	"github.com/authorizerdev/authorizer/server/sessionstore"
	"github.com/authorizerdev/authorizer/server/token"
	"github.com/authorizerdev/authorizer/server/utils"
)

// SignupResolver is a resolver for signup mutation
func SignupResolver(ctx context.Context, params model.SignUpInput) (*model.AuthResponse, error) {
	var res *model.AuthResponse

	gc, err := utils.GinContextFromContext(ctx)
	if err != nil {
		log.Debug("Failed to get GinContext", err)
		return res, err
	}

	if envstore.EnvStoreObj.GetBoolStoreEnvVariable(constants.EnvKeyDisableSignUp) {
		log.Debug("Signup is disabled.")
		return res, fmt.Errorf(`signup is disabled for this instance`)
	}

	if envstore.EnvStoreObj.GetBoolStoreEnvVariable(constants.EnvKeyDisableBasicAuthentication) {
		log.Debug("Basic authentication is disabled.")
		return res, fmt.Errorf(`basic authentication is disabled for this instance`)
	}

	if params.ConfirmPassword != params.Password {
		log.Debug("Passwords do not match.")
		return res, fmt.Errorf(`password and confirm password does not match`)
	}

	if !utils.IsValidPassword(params.Password) {
		log.Debug("Invalid password")
		return res, fmt.Errorf(`password is not valid. It needs to be at least 6 characters long and contain at least one number, one uppercase letter, one lowercase letter and one special character`)
	}

	params.Email = strings.ToLower(params.Email)

	if !utils.IsValidEmail(params.Email) {
		log.Debug("Invalid email:", params.Email)
		return res, fmt.Errorf(`invalid email address`)
	}

	log := log.WithFields(log.Fields{
		"email": params.Email,
	})
	// find user with email
	existingUser, err := db.Provider.GetUserByEmail(params.Email)
	if err != nil {
		log.Debug("Failed to get user by email:", err)
	}

	if existingUser.EmailVerifiedAt != nil {
		// email is verified
		log.Debug("Email is already verified and signed up.")
		return res, fmt.Errorf(`%s has already signed up`, params.Email)
	} else if existingUser.ID != "" && existingUser.EmailVerifiedAt == nil {
		log.Debug("Email is already signed up. Verification pending...")
		return res, fmt.Errorf("%s has already signed up. please complete the email verification process or reset the password", params.Email)
	}

	inputRoles := []string{}

	if len(params.Roles) > 0 {
		// check if roles exists
		if !utils.IsValidRoles(envstore.EnvStoreObj.GetSliceStoreEnvVariable(constants.EnvKeyRoles), params.Roles) {
			log.Debug("Invalid roles", params.Roles)
			return res, fmt.Errorf(`invalid roles`)
		} else {
			inputRoles = params.Roles
		}
	} else {
		inputRoles = envstore.EnvStoreObj.GetSliceStoreEnvVariable(constants.EnvKeyDefaultRoles)
	}

	user := models.User{
		Email: params.Email,
	}

	user.Roles = strings.Join(inputRoles, ",")

	password, _ := crypto.EncryptPassword(params.Password)
	user.Password = &password

	if params.GivenName != nil {
		user.GivenName = params.GivenName
	}

	if params.FamilyName != nil {
		user.FamilyName = params.FamilyName
	}

	if params.MiddleName != nil {
		user.MiddleName = params.MiddleName
	}

	if params.Nickname != nil {
		user.Nickname = params.Nickname
	}

	if params.Gender != nil {
		user.Gender = params.Gender
	}

	if params.Birthdate != nil {
		user.Birthdate = params.Birthdate
	}

	if params.PhoneNumber != nil {
		user.PhoneNumber = params.PhoneNumber
	}

	if params.Picture != nil {
		user.Picture = params.Picture
	}

	user.SignupMethods = constants.SignupMethodBasicAuth
	if envstore.EnvStoreObj.GetBoolStoreEnvVariable(constants.EnvKeyDisableEmailVerification) {
		now := time.Now().Unix()
		user.EmailVerifiedAt = &now
	}
	user, err = db.Provider.AddUser(user)
	if err != nil {
		log.Debug("Failed to add user:", err)
		return res, err
	}
	roles := strings.Split(user.Roles, ",")
	userToReturn := user.AsAPIUser()

	hostname := utils.GetHost(gc)
	if !envstore.EnvStoreObj.GetBoolStoreEnvVariable(constants.EnvKeyDisableEmailVerification) {
		// insert verification request
		_, nonceHash, err := utils.GenerateNonce()
		if err != nil {
			log.Debug("Failed to generate nonce:", err)
			return res, err
		}
		verificationType := constants.VerificationTypeBasicAuthSignup
		redirectURL := utils.GetAppURL(gc)
		if params.RedirectURI != nil {
			redirectURL = *params.RedirectURI
		}
		verificationToken, err := token.CreateVerificationToken(params.Email, verificationType, hostname, nonceHash, redirectURL)
		if err != nil {
			log.Debug("Failed to create verification token:", err)
			return res, err
		}
		_, err = db.Provider.AddVerificationRequest(models.VerificationRequest{
			Token:       verificationToken,
			Identifier:  verificationType,
			ExpiresAt:   time.Now().Add(time.Minute * 30).Unix(),
			Email:       params.Email,
			Nonce:       nonceHash,
			RedirectURI: redirectURL,
		})
		if err != nil {
			log.Debug("Failed to add verification request:", err)
			return res, err
		}

		// exec it as go routin so that we can reduce the api latency
		go email.SendVerificationMail(params.Email, verificationToken, hostname)

		res = &model.AuthResponse{
			Message: `Verification email has been sent. Please check your inbox`,
			User:    userToReturn,
		}
	} else {
		scope := []string{"openid", "email", "profile"}
		if params.Scope != nil && len(scope) > 0 {
			scope = params.Scope
		}

		authToken, err := token.CreateAuthToken(gc, user, roles, scope)
		if err != nil {
			log.Debug("Failed to create auth token:", err)
			return res, err
		}

		sessionstore.SetState(authToken.FingerPrintHash, authToken.FingerPrint+"@"+user.ID)
		cookie.SetSession(gc, authToken.FingerPrintHash)
		go db.Provider.AddSession(models.Session{
			UserID:    user.ID,
			UserAgent: utils.GetUserAgent(gc.Request),
			IP:        utils.GetIP(gc.Request),
		})

		expiresIn := authToken.AccessToken.ExpiresAt - time.Now().Unix()
		if expiresIn <= 0 {
			expiresIn = 1
		}

		res = &model.AuthResponse{
			Message:     `Signed up successfully.`,
			AccessToken: &authToken.AccessToken.Token,
			ExpiresIn:   &expiresIn,
			User:        userToReturn,
		}
	}

	return res, nil
}
