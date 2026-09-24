# scimux privacy policy

## Scope

scimux is an application you run on your own computer to supervise
AI agent command-line tools.

This policy describes how the scimux application handles information.
The agent tools, model providers, and any hosted services you use have
their own privacy policies and terms.

## Information stored on your devices

scimux stores information needed to display, supervise, and preserve
your work. This includes:

- Conversation prompts and responses, tool activity, approval decisions, timestamps, and available usage information.
- Chat titles, working-directory paths, model choices, and other chat metadata.
- Uploaded attachments and copies of files imported into chat history.
- Notes, bookmarks, saved references, preferences, and settings.
- Remote-pairing identities and configuration when remote access is enabled.

Application records are stored in your configured data directory,
which defaults to `~/.scimux`. Your browser also stores information
such as unsent drafts, preferences, and cached interface state.

Your agent tools may keep their own records outside scimux's data
directory.

## How information is used and shared

scimux uses this information to run the interface, deliver your
instructions, supervise agents, and provide history, search, notes,
and references.

When you send a prompt, scimux passes it to your selected agent tool.
That tool may send prompts, files, tool results, and other context to
its configured model provider or connected services. What it accesses
and sends depends on its configuration, permissions, and your actions.

Sending content from one chat to another can disclose it to a different
provider. Information already present in a working directory may also
be accessed by an agent.

scimux does not read agent credential files or provide vendor login.
You install and authenticate the agent tools yourself.

The scimux application does not automatically upload your conversation
history to the project maintainer or include advertising or behavioral
analytics. The scimux project does not use your conversation history
to train, fine-tune, or distill models.

Your providers' processing and training practices depend on their
terms, your plan, and settings.

## Muse Contributor models

Meta may retain content submitted to Muse Contributor models and use
it for model training.

Do not use Contributor models with confidential, sensitive, or personal
information. This includes prompts, files, and tool results the agent
can read. Acknowledging the notice does not make prohibited submissions
permissible.

Choose a Standard model if your content must not be used for training.
Standard models remain subject to Meta's processing and retention terms.

scimux does not automatically select Contributor models. Starting a
Contributor chat or fork requires acknowledging its data-use notice.
This acknowledgment is separate from the setting that permits Muse's
quota-consuming approval judge.

See Meta's terms:
https://dev.meta.ai/legal/terms-of-service

## Network connections

Checking for scimux or harness updates contacts GitHub or the relevant
public release service. Installing an update downloads release files.
These services receive ordinary connection information, such as your
IP address. These requests do not include your chat contents.

Following an external link contacts the linked website.

If you explicitly enable experimental remote access, the viewer,
rendezvous, and STUN services participate in establishing the connection.
They receive connection or pairing information needed for that role,
including network addresses and protocol identifiers.

Conversation access then takes place over an encrypted connection
between your computer and the paired device. The rendezvous service
does not relay conversation content. The hosted viewer and its delivery
infrastructure remain trusted components.

Hosted-service logging and retention are separate from the application's
local storage.

## Who can access your information

Anyone who can reach scimux's local HTTP listener can access its
interface, conversations, and controls. It has no application login.
The default listener is restricted to your computer's loopback address.

Paired devices can access information and controls through remote
access. People or software with sufficient access to your computer,
browser profile, or backups may also access stored records.

Revoking a device prevents future authorized connections; it does not
erase information already copied to that device.

## Retention and deletion

scimux preserves history until you remove the stored data. Deleting a
chat or note in the interface archives its records rather than securely
erasing them. Clearing a conversation starts a new page and preserves
earlier history. Notes may retain earlier versions.

Storage limits prevent additional writes; they do not automatically
erase older records.

Removing local data does not remove copies held by agent tools,
providers, other devices, or backups. Browser data must be managed
separately through your browser's controls. Provider deletion requests
must be directed to the relevant provider.

## Contact

For privacy questions about scimux, contact the maintainer at
security@scimux.com. Do not include credentials or conversation contents
unless they are necessary and you have agreed on a suitable way to
share them.
