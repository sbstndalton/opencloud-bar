# Signing the Windows build

Windows Smart App Control blocks unsigned programs. The workflow `.github/workflows/windows-signed.yml`
builds the Windows binaries and signs them with **Azure Artifact Signing** (formerly Trusted Signing).
GitHub signs in to Azure with OIDC, so no password, key or certificate is stored in the repository.

## One-time Azure setup (done by the account owner)

1. **Azure account.** Sign up at <https://azure.microsoft.com> and add a payment method.
2. **Resource provider.** In the subscription, open *Resource providers* and register `Microsoft.CodeSigning`.
3. **Signing account.** Create an *Artifact Signing account* in a new resource group, for example `opencloud-bar-signing`.
   - Choose the **Basic** tier. Billing starts as soon as the account exists and isn't prorated.
   - Note the region's endpoint, for example East US is `https://eus.codesigning.azure.net/`.
4. **Identity validation.**
   - On the signing account, give yourself the *Identity Verifier* role (Access control (IAM)).
   - Then go to *Identity validations* > *New* > **Public** > **Individual**, and complete Microsoft's ID check (government ID and selfie).
   - Wait until the validation shows *Completed*. This can take from minutes to a few days.
5. **Certificate profile.** Go to *Certificate profiles* > *Create* > **Public Trust**, and pick the completed validation.
6. **GitHub login.**
   - In *Microsoft Entra ID* > *App registrations*, create a new registration, for example `opencloud-bar-github-signing`.
   - Under *Certificates & secrets* > *Federated credentials*, add one with these settings:
     - scenario **GitHub Actions**
     - organization `sbstndalton`
     - repository `opencloud-bar`
     - entity **Environment**
     - environment name `signing`
7. **Permission to sign.** On the signing account, open *Access control (IAM)* and assign the *Certificate Profile Signer* role to that app registration.

## Connect it to the repository

Create the environment and variables. They're IDs and names, not secrets:

```bash
gh api -X PUT repos/sbstndalton/opencloud-bar/environments/signing
gh variable set AZURE_CLIENT_ID       -R sbstndalton/opencloud-bar -b <app (client) id>
gh variable set AZURE_TENANT_ID       -R sbstndalton/opencloud-bar -b <tenant id>
gh variable set AZURE_SUBSCRIPTION_ID -R sbstndalton/opencloud-bar -b <subscription id>
gh variable set SIGNING_ENDPOINT      -R sbstndalton/opencloud-bar -b https://eus.codesigning.azure.net/
gh variable set SIGNING_ACCOUNT       -R sbstndalton/opencloud-bar -b <signing account name>
gh variable set SIGNING_PROFILE       -R sbstndalton/opencloud-bar -b <certificate profile name>
```

## Build

To build, do either of the following:
- Run **Actions > Windows (signed) > Run workflow**. The signed `opencloud-bar.exe` (tray) and `opencloud-bar-cli.exe` are attached to the run as an artifact.
- Push a `v*` tag. The two files are also attached to that release.

The workflow fails with the signature status in its log if signing didn't produce a valid signature.
