import React, { Component } from 'react';
import PropTypes from 'prop-types';
import styles from './SecretKey.module.scss';
import StellarSdk from 'stellar-sdk'
import grid from '../../_styles/grid.module.scss';
import Button from '../../atoms/Button/Button';
import Label from '../../atoms/Label/Label';
import Input from '../../atoms/Input/Input';

class SecretKey extends Component {
  static propTypes = {
    label: PropTypes.string.isRequired,
    isTestNet: PropTypes.bool.isRequired,
    secret: PropTypes.string.isRequired,
    onSecretChange: PropTypes.func.isRequired,
    onError: PropTypes.func.isRequired,
    onNewKeyClick: PropTypes.func,
    optional: PropTypes.bool,
  };

  render() {
    let inputElem = (<Input
      value={this.props.secret}
      type="string"
      onChange={(event) => { this.props.onSecretChange(event) }}
      error={this.props.onError}
      />);

    let secretElem = inputElem;
    if (this.props.isTestNet) {
      secretElem = (
        <div className={grid.row}>
          <div className={grid.col90p}>
            {inputElem}
          </div>
          <div className={grid.col10p}>
            <Button 
              icon="refresh"
              size="small"
              hsize="round"
              loading={false}
              onClick={this.props.onNewKeyClick}
              />
          </div>
        </div>
      );
    }

    let label = (<Label>{this.props.label}</Label>);
    if (this.props.optional) {
      label = (<Label optional>{this.props.label}</Label>);
    }

    let pubkeyElem = ""
    if (this.props.secret !== "") {
      let pubkeypair = StellarSdk.Keypair.fromSecret(this.props.secret);
      pubkeyElem = (
        <div>
          <span className={styles.pubkeyLabel}>PubKey: </span><span className={styles.pubkey}>{pubkeypair.publicKey()}</span>
        </div>
      );
    }

    return (
      <div>
        {label}
        {pubkeyElem}
        {secretElem}
      </div>
    );
  }
}

export default SecretKey;