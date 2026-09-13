import type { IdentityPort } from '../modules/identity/model';
import type { UserRow } from '../modules/users/model';
import type { ApiKeyRow } from '../modules/apikeys/model';
import type { ProviderRow } from '../modules/providers/model';
import type { ListPort } from '../shared/ports';

export interface ApplicationPorts {
  identity: IdentityPort;
  users: ListPort<UserRow>;
  apiKeys: ListPort<ApiKeyRow>;
  providers: ListPort<ProviderRow>;
}
