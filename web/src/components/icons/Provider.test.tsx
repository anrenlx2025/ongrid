import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ModelIcon, ProviderIcon } from './Provider';

describe('ProviderIcon', () => {
  it.each([
    ['minimax', 'MiniMax-M2.7', '#e73562'],
    ['xiaomi', 'mimo-v2.5-pro', '#000000'],
  ])('renders the %s brand for provider and model', (provider, model, color) => {
    const providerView = render(<ProviderIcon provider={provider} />);
    expect(providerView.container.querySelector(`[fill="${color}"]`)).toBeInTheDocument();
    providerView.unmount();

    const modelView = render(<ModelIcon provider="custom" model={model} />);
    expect(modelView.container.querySelector(`[fill="${color}"]`)).toBeInTheDocument();
  });
});
